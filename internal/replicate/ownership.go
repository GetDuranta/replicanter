package replicate

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/getduranta/replicanter/internal/ownership"
)

// Ownership reclamation.
//
// replicanter drives the storage library as real root against stores owned by
// unprivileged builders, so every file the library creates on our behalf lands
// as root:root — including layers.json and images.json, which are mode 0600.
// Left that way, the store's own user can no longer read its own store. The
// library offers no hook for this (it derives file ownership from the process's
// id mappings), so we hand the files back ourselves, immediately after each
// commit rather than once at the end: the window during which a concurrent
// podman in the destination cannot read its metadata must be as short as
// possible.
//
// Only entries owned by uid 0 or gid 0 are touched. In a rootless store that
// ownership is never legitimate — it can only be something this process made —
// so the rule is safe to apply broadly and is also self-repairing for stores
// damaged by an earlier run.

// storeMetadataPaths lists the store-wide files and directories the library
// rewrites as it saves. layers.json and images.json in particular are replaced
// (atomic write + rename) on every mutation, so they revert to root:root after
// each one and must be reclaimed after each one.
func (s *Store) storeMetadataPaths() []string {
	return []string{
		s.graphRoot,
		s.driverDir(),
		s.linkDir(),
		filepath.Join(s.driverDir(), "staging"),  // in-flight staged layers
		filepath.Join(s.driverDir(), "tempdirs"), // layers pending deletion
		s.layersDir(),
		filepath.Join(s.layersDir(), "layers.json"),
		filepath.Join(s.layersDir(), "volatile-layers.json"),
		s.imagesDir(),
		filepath.Join(s.imagesDir(), "images.json"),
		filepath.Join(s.imagesDir(), "volatile-images.json"),
	}
}

// reclaimStoreMetadata hands back the store-wide metadata files. Cheap (a
// handful of lstats), so it runs after every commit and once per store per run.
func reclaimStoreMetadata(s *Store) error {
	for _, path := range s.storeMetadataPaths() {
		if _, err := s.owner.ClaimIfRoot(path); err != nil {
			return err
		}
	}
	return nil
}

// reclaimLayer hands back everything the library just created for a freshly
// committed layer: the driver directory and its bookkeeping files, the symlink
// the driver added to the link farm, the tar-split, and the layer's big-data
// directory.
//
// The layer's diff/ tree is deliberately not walked. Its ownership was set by
// the reflink clone, translated from the source store's owner to this store's
// (see ownership.Shift); the ids in there are in-layer identities, not ours to
// reclaim.
func reclaimLayer(s *Store, layerID string) error {
	dir := s.layerDir(layerID)
	if err := claimDirShallow(s.owner, dir); err != nil {
		return fmt.Errorf("layer %s driver dir: %w", layerID, err)
	}

	// <graphroot>/overlay/l/<LINK> -> ../<id>/diff, named by the layer's "link"
	// file. Absent for a layer the driver has not linked (nothing to reclaim).
	link, err := os.ReadFile(filepath.Join(dir, "link"))
	switch {
	case err == nil:
		if name := strings.TrimSpace(string(link)); name != "" {
			if _, err := s.owner.ClaimIfRoot(filepath.Join(s.linkDir(), name)); err != nil {
				return fmt.Errorf("layer %s link farm entry: %w", layerID, err)
			}
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return fmt.Errorf("reading link file of layer %s: %w", layerID, err)
	}

	if _, err := s.owner.ClaimIfRoot(s.layerTarSplitPath(layerID)); err != nil {
		return fmt.Errorf("layer %s tar-split: %w", layerID, err)
	}
	if err := claimTree(s.owner, s.layerMetadataDir(layerID)); err != nil {
		return fmt.Errorf("layer %s metadata dir: %w", layerID, err)
	}
	return reclaimStoreMetadata(s)
}

// reclaimImage hands back a freshly created image's metadata directory (its
// manifest, config and other big-data items) and the store-wide files.
func reclaimImage(s *Store, imageID string) error {
	if err := claimTree(s.owner, s.imageDir(imageID)); err != nil {
		return fmt.Errorf("image %s metadata dir: %w", imageID, err)
	}
	return reclaimStoreMetadata(s)
}

// reclaimStore sweeps the whole store for anything still owned by root. It is
// the catch-all behind the targeted per-layer and per-image reclaim: it covers
// metadata this tool does not know by name, temporary files left by an
// interrupted atomic write, and stores damaged by an earlier run.
//
// Cost is bounded by the prune rules in walkMetadata: no layer content is ever
// walked, so this is a few tens of thousands of lstats on a large store.
func reclaimStore(s *Store, log *slog.Logger) error {
	reclaimed := 0
	err := filepath.WalkDir(s.graphRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Podman may be pruning concurrently; a vanished entry is not ours
			// to worry about.
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		changed, err := s.owner.ClaimIfRoot(path)
		if err != nil {
			return err
		}
		if changed {
			reclaimed++
		}
		if d.IsDir() && !walkMetadata(s.graphRoot, path) {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return err
	}
	if reclaimed > 0 {
		log.Info("reclaimed root-owned files in store sweep",
			"count", reclaimed, "owner", s.owner, "graphroot", s.graphRoot)
	}
	return nil
}

// walkMetadata reports whether the ownership sweep should descend into dir (an
// absolute path inside graphRoot). It walks store metadata and stops at every
// directory that holds layer *content*:
//
//   - overlay/<id>/diff — the layer itself. Its ids are in-layer identities,
//     set by the reflink clone; uid 0 is never valid there.
//   - overlay/<id>/merged — an active overlay mount point. Descending it would
//     mean walking a running container's entire filesystem.
//   - overlay/staging, overlay/tempdirs — layer content in flight or pending
//     deletion, same reasoning as diff.
//   - everything outside overlay/, overlay-images/ and overlay-layers/ — that
//     is libpod's own state (volumes, networks, secrets, db.sql), which this
//     tool never writes to.
func walkMetadata(graphRoot, dir string) bool {
	rel, err := filepath.Rel(graphRoot, dir)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	parts := strings.Split(rel, string(filepath.Separator))
	switch parts[0] {
	case imagesSubdir, layersSubdir:
		return true // pure metadata, all of it
	case driverSubdir:
		switch len(parts) {
		case 1: // overlay/
			return true
		case 2: // overlay/<layer id> and overlay/l are metadata; the rest is content
			return parts[1] != "staging" && parts[1] != "tempdirs"
		default: // overlay/<id>/diff, /merged, /work, … — never walked
			return false
		}
	default:
		return false
	}
}

// claimDirShallow hands dir and its immediate children to owner, without
// descending into any of them.
func claimDirShallow(owner ownership.Owner, dir string) error {
	if _, err := owner.ClaimIfRoot(dir); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if _, err := owner.ClaimIfRoot(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// claimTree hands root and everything below it to owner. Intended for the small
// metadata directories (an image's big-data, a layer's big-data); a missing
// root is not an error.
func claimTree(owner ownership.Owner, root string) error {
	return filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		_, err = owner.ClaimIfRoot(path)
		return err
	})
}
