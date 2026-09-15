package replicate

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/getduranta/replicanter/internal/ownership"
	"github.com/getduranta/replicanter/internal/reflink"
	"github.com/opencontainers/go-digest"
	"go.podman.io/storage"
	graphdriver "go.podman.io/storage/drivers"
	"go.podman.io/storage/pkg/idtools"
	"go.podman.io/storage/types"
)

// Stats summarizes one synchronization run.
type Stats struct {
	LayersCopied  int
	LayersSkipped int
	ImagesCopied  int
	ImagesSkipped int
}

// Options controls a synchronization run.
type Options struct {
	// Verify recomputes the DiffID of each freshly reflinked layer and asserts
	// it equals the source's recorded UncompressedDigest. Slow (it re-tars the
	// content); intended for CI, gated off in production.
	Verify bool
	// DryRun logs what would be replicated without mutating any store.
	DryRun bool
	Log    *slog.Logger
}

func (o *Options) logger() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

// Sync makes every store hold the union of all images across the set: for each
// image present in any store but missing from another, the image and its layer
// chain are replicated into the store that lacks it. Layers are always created
// bottom-up (parents before children). The operation is idempotent: images and
// layers already present are skipped.
//
// Caller is responsible for holding the global replication lock (see internal/
// lock) before calling Sync.
func Sync(stores []*Store, opts Options) (Stats, error) {
	var stats Stats
	log := opts.logger()

	for _, dst := range stores {
		err := syncInto(dst, stores, opts, &stats)

		// Reclaim ownership even when the run failed: a store left holding
		// root-owned metadata is unreadable by its own user (see ownership.go).
		if rerr := reclaimAfterSync(dst, opts); rerr != nil {
			rerr = fmt.Errorf("reclaiming ownership in %s: %w", dst.GraphRoot(), rerr)
			if err == nil {
				err = rerr
			} else {
				log.Error("reclaiming ownership after a failed sync", "err", rerr)
			}
		}
		if err != nil {
			return stats, err
		}
	}

	log.Info("sync complete",
		"layers_copied", stats.LayersCopied, "layers_skipped", stats.LayersSkipped,
		"images_copied", stats.ImagesCopied, "images_skipped", stats.ImagesSkipped)
	return stats, nil
}

// syncInto replicates into dst every image held by any other store.
func syncInto(dst *Store, stores []*Store, opts Options, stats *Stats) error {
	for _, src := range stores {
		if src == dst {
			continue
		}
		images, err := src.Images()
		if err != nil {
			return fmt.Errorf("listing images in %s: %w", src.GraphRoot(), err)
		}
		for i := range images {
			img := &images[i]
			if err := replicateImage(src, dst, img, opts, stats); err != nil {
				return fmt.Errorf("replicating image %s (%v) %s -> %s: %w",
					img.ID, img.Names, src.GraphRoot(), dst.GraphRoot(), err)
			}
		}
	}
	return nil
}

// reclaimAfterSync closes out a store's turn as a destination with the full
// ownership sweep. It runs unconditionally, even for a store nothing was
// replicated into: the sweep costs a fraction of a second (it never walks layer
// content), and running it every time is what makes a store damaged by an
// earlier run — or by an interrupted one — repair itself.
func reclaimAfterSync(dst *Store, opts Options) error {
	if opts.DryRun {
		return nil // --dry-run mutates nothing, ownership included
	}
	return reclaimStore(dst, opts.logger())
}

func replicateImage(src, dst *Store, img *storage.Image, opts Options, stats *Stats) error {
	log := opts.logger()

	if _, err := dst.Image(img.ID); err == nil {
		// Already present. Idempotent no-op — the image-level reference (and
		// thus layer refcounts) already exists in dst.
		stats.ImagesSkipped++
		return nil
	} else if !errors.Is(err, storage.ErrImageUnknown) {
		return fmt.Errorf("checking dst for image %s: %w", img.ID, err)
	}

	chain, err := layerChain(src, img.TopLayer)
	if err != nil {
		return err
	}

	for _, layer := range chain {
		if err := replicateLayer(src, dst, layer, opts, stats); err != nil {
			return err
		}
	}

	if opts.DryRun {
		log.Info("would create image", "id", img.ID, "names", img.Names, "dst", dst.GraphRoot())
		stats.ImagesCopied++
		return nil
	}

	if err := createImage(src, dst, img, log); err != nil {
		return err
	}
	if err := reclaimImage(dst, img.ID); err != nil {
		return fmt.Errorf("reclaiming ownership of image %s: %w", img.ID, err)
	}
	stats.ImagesCopied++
	log.Info("replicated image", "id", img.ID, "names", img.Names, "dst", dst.GraphRoot())
	return nil
}

// layerChain returns the layers from topLayer down to the root, ordered
// bottom-up (root first) so each layer can be created after its parent.
func layerChain(src *Store, topLayer string) ([]*storage.Layer, error) {
	var topDown []*storage.Layer
	for id := topLayer; id != ""; {
		layer, err := src.Layer(id)
		if err != nil {
			return nil, fmt.Errorf("resolving layer %s: %w", id, err)
		}
		topDown = append(topDown, layer)
		id = layer.Parent
	}
	// Reverse into bottom-up order.
	for i, j := 0, len(topDown)-1; i < j; i, j = i+1, j-1 {
		topDown[i], topDown[j] = topDown[j], topDown[i]
	}
	return topDown, nil
}

func replicateLayer(src, dst *Store, srcLayer *storage.Layer, opts Options, stats *Stats) error {
	log := opts.logger()

	if _, err := dst.Layer(srcLayer.ID); err == nil {
		stats.LayersSkipped++
		return nil // already present, content already shared/copied
	} else if !errors.Is(err, storage.ErrLayerUnknown) {
		return fmt.Errorf("checking dst for layer %s: %w", srcLayer.ID, err)
	}

	srcDiff := src.layerDiffDir(srcLayer.ID)
	if fi, err := os.Stat(srcDiff); err != nil || !fi.IsDir() {
		return fmt.Errorf("source layer %s diff dir %q not a directory (composefs in use?): %v",
			srcLayer.ID, srcDiff, err)
	}

	if opts.DryRun {
		log.Info("would replicate layer", "id", srcLayer.ID, "parent", srcLayer.Parent,
			"uncompressed_size", srcLayer.UncompressedSize, "dst", dst.GraphRoot())
		stats.LayersCopied++
		return nil
	}

	srcTarSplit := src.layerTarSplitPath(srcLayer.ID)
	if _, err := os.Stat(srcTarSplit); err != nil {
		srcTarSplit = "" // no tar-split for this layer; DiffID won't be reproducible
	}

	differ := &reflinkDiffer{
		srcDiffDir:         srcDiff,
		ownerShift:         dst.shiftFrom(src),
		srcTarSplitGzPath:  srcTarSplit,
		uncompressedDigest: srcLayer.UncompressedDigest,
		uncompressedSize:   srcLayer.UncompressedSize,
		compressedDigest:   srcLayer.CompressedDigest,
		tocDigest:          srcLayer.TOCDigest,
		uids:               srcLayer.UIDs,
		gids:               srcLayer.GIDs,
	}

	diffOpts := &graphdriver.ApplyDiffWithDifferOpts{
		ApplyDiffOpts: graphdriver.ApplyDiffOpts{
			Mappings:   idtools.NewIDMappingsFromMaps(srcLayer.UIDMap, srcLayer.GIDMap),
			MountLabel: srcLayer.MountLabel,
		},
	}

	// PrepareStagedLayer runs our differ (the reflink) into a staging dir under
	// the dst graphroot. Nothing is visible to dst podman yet.
	diffOutput, err := dst.PrepareStagedLayer(diffOpts, differ)
	if err != nil {
		return fmt.Errorf("staging layer %s: %w", srcLayer.ID, err)
	}

	// From here, either commit (ApplyStagedLayer) or clean up the staging dir.
	committed := false
	defer func() {
		if !committed {
			if cerr := dst.CleanupStagedLayer(diffOutput); cerr != nil {
				log.Error("cleaning up staged layer", "id", srcLayer.ID, "err", cerr)
			}
		}
	}()

	layerOpts := &storage.LayerOptions{
		IDMappingOptions: types.IDMappingOptions{
			HostUIDMapping: len(srcLayer.UIDMap) == 0,
			HostGIDMapping: len(srcLayer.GIDMap) == 0,
			UIDMap:         srcLayer.UIDMap,
			GIDMap:         srcLayer.GIDMap,
		},
		// Transcribe the recorded digests so they remain truthful; Size is
		// carried by diffOutput (see reflinkDiffer / applyDiffFromStagingDirectory).
		UncompressedDigest: srcLayer.UncompressedDigest,
		OriginalDigest:     srcLayer.CompressedDigest,
	}
	if srcLayer.CompressedSize > 0 {
		sz := srcLayer.CompressedSize
		layerOpts.OriginalSize = &sz
	}

	// ApplyStagedLayer creates the layer record and atomically renames the
	// staged content into place, all under the destination store's write lock.
	if _, err := dst.ApplyStagedLayer(storage.ApplyStagedLayerOptions{
		ID:           srcLayer.ID,
		ParentLayer:  srcLayer.Parent,
		Names:        srcLayer.Names,
		MountLabel:   srcLayer.MountLabel,
		LayerOptions: layerOpts,
		DiffOutput:   diffOutput,
		DiffOptions:  diffOpts,
	}); err != nil {
		return fmt.Errorf("committing layer %s: %w", srcLayer.ID, err)
	}
	committed = true

	// Hand the layer's metadata back to the destination's owner before anything
	// else: until this runs, dst's own user cannot read its layers.json.
	if err := reclaimLayer(dst, srcLayer.ID); err != nil {
		return fmt.Errorf("reclaiming ownership of layer %s: %w", srcLayer.ID, err)
	}
	stats.LayersCopied++

	if opts.Verify {
		if srcTarSplit == "" {
			log.Warn("skipping DiffID verification: source layer has no tar-split", "id", srcLayer.ID)
		} else if err := verifyLayer(dst, srcLayer); err != nil {
			return fmt.Errorf("verify layer %s: %w", srcLayer.ID, err)
		}
	}

	log.Info("replicated layer", "id", srcLayer.ID, "parent", srcLayer.Parent,
		"uncompressed_size", srcLayer.UncompressedSize, "dst", dst.GraphRoot(),
		"owner_shift", dst.shiftFrom(src))
	return nil
}

// verifyLayer recomputes the DiffID (sha256 of the uncompressed diff tar) of
// the freshly committed destination layer using the library's own diff
// serialization, and asserts it equals the source's recorded UncompressedDigest.
// This is the expensive integrity gate (re-tars content); for CI, not prod.
func verifyLayer(dst *Store, srcLayer *storage.Layer) error {
	if srcLayer.UncompressedDigest == "" {
		return nil // nothing recorded to verify against
	}
	rc, err := dst.Diff("", srcLayer.ID, nil)
	if err != nil {
		return fmt.Errorf("opening diff: %w", err)
	}
	defer rc.Close()

	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return fmt.Errorf("hashing diff: %w", err)
	}
	got := digest.NewDigestFromEncoded(digest.SHA256, fmt.Sprintf("%x", h.Sum(nil)))
	if got != srcLayer.UncompressedDigest {
		return fmt.Errorf("DiffID mismatch: recomputed %s, source recorded %s", got, srcLayer.UncompressedDigest)
	}
	return nil
}

// createImage creates the image record in dst referencing the (already
// replicated) top layer, and copies every image big-data item (manifest,
// config, signatures, …) so the image is complete and self-consistent.
func createImage(src, dst *Store, img *storage.Image, log *slog.Logger) error {
	imgOpts := &storage.ImageOptions{
		CreationDate: img.Created,
		Digest:       img.Digest,
		Digests:      img.Digests,
		Metadata:     img.Metadata,
		NamesHistory: img.NamesHistory,
	}

	names, err := availableNames(dst, img, log)
	if err != nil {
		return err
	}

	created, err := dst.CreateImage(img.ID, names, img.TopLayer, img.Metadata, imgOpts)
	if err != nil {
		return fmt.Errorf("creating image record: %w", err)
	}

	bigDataKeys, err := src.ListImageBigData(img.ID)
	if err != nil {
		return fmt.Errorf("listing image big-data: %w", err)
	}
	for _, key := range bigDataKeys {
		data, err := src.ImageBigData(img.ID, key)
		if err != nil {
			return fmt.Errorf("reading image big-data %q: %w", key, err)
		}
		// For manifest big-data keys the library needs a callback to derive the
		// manifest digest (used to index the image by digest). A container
		// image manifest's digest is simply the sha256 of its raw bytes, so
		// digest.FromBytes is correct; the callback is ignored for non-manifest
		// keys.
		if err := dst.SetImageBigData(created.ID, key, data, manifestDigest); err != nil {
			return fmt.Errorf("writing image big-data %q: %w", key, err)
		}
	}
	return nil
}

// imageResolver looks up an image by ID or name. *Store satisfies it via the
// embedded storage.Store; availableNames depends on this narrow interface so it
// can be unit-tested without standing up a real overlay store.
type imageResolver interface {
	Image(id string) (*storage.Image, error)
	GraphRoot() string
}

// availableNames returns the subset of the source image's names that are not
// already claimed by a different image in dst.
//
// Container images are content-addressed: the image is fully identified by its
// ID (derived from its config) and its layers by DiffID, so a replicated image
// is complete and its layers are shared/reflinked by content hash whether or
// not it carries a tag. A tag, by contrast, is a mutable human-facing pointer
// owned by the build system. When a name like "localhost/app_db:latest" is
// already claimed by a different image in dst — typically because that tag was
// rebuilt to new content and this store still holds the previous one — we
// decline to steal the tag and replicate the image untagged rather than fail
// the whole sync. Moving tags is not replicanter's call; getting the content
// onto every store is. The skipped name is logged.
func availableNames(dst imageResolver, img *storage.Image, log *slog.Logger) ([]string, error) {
	var names []string
	for _, name := range img.Names {
		existing, err := dst.Image(name)
		if errors.Is(err, storage.ErrImageUnknown) {
			names = append(names, name)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("checking dst for name %q: %w", name, err)
		}
		if existing.ID == img.ID {
			names = append(names, name)
			continue
		}
		log.Warn("skipping name already claimed by another image in dst; replicating untagged",
			"name", name, "image", img.ID, "held_by", existing.ID, "dst", dst.GraphRoot())
	}
	return names, nil
}

// manifestDigest computes the digest the storage library records for a manifest
// big-data item. A manifest's digest is the sha256 of its exact bytes.
func manifestDigest(data []byte) (digest.Digest, error) {
	return digest.FromBytes(data), nil
}

// cloneTreeInto reflinks the directory tree src into dest, translating the
// source store owner's identity into the destination's as it goes. It is the
// bridge from the differ to the reflink package; kept here so the differ stays
// free of filesystem-walk concerns.
func cloneTreeInto(src, dest string, shift ownership.Shift) error {
	return reflink.CloneTree(src, dest, shift)
}
