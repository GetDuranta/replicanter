// Package replicate moves container image layers between two
// containers/storage stores on the same btrfs volume by reflinking layer
// content (copy-on-write) instead of serializing/deserializing tar streams,
// while routing all metadata and locking through the official
// go.podman.io/storage API so the tool is a safe second client.
package replicate

import (
	"fmt"
	"path/filepath"

	"github.com/getduranta/replicanter/internal/ownership"
	"go.podman.io/storage"
	"go.podman.io/storage/types"
)

// On-disk layout of an overlay store under its graphroot. The driver name is
// part of the directory names, and this tool only ever opens overlay stores.
const (
	driverSubdir = "overlay"        // layer content + driver bookkeeping
	layersSubdir = "overlay-layers" // layer metadata, big-data and tar-splits
	imagesSubdir = "overlay-images" // image metadata and big-data
	linkSubdir   = "l"              // driver's short-name symlink farm
)

// Store wraps an opened containers/storage store for a single graphroot and
// remembers the graphroot path so we can locate raw layer content on disk for
// reflinking.
type Store struct {
	storage.Store
	graphRoot string
	owner     ownership.Owner
}

// Open opens (or initializes) the overlay store rooted at graphRoot, using
// runRoot for runtime state. composefs is force-disabled: the reflink Differ
// produces a plain directory-format layer, and the overlay driver's composefs
// path expects fs-verity/TOC artifacts this tool does not generate. composefs
// support is explicitly out of scope (see PLAN.md).
//
// runRoot must be a private, writable path owned by the caller (the daemon
// runs as root); it must NOT point at any builder's per-user runroot, so that
// each builder keeps its own `podman ps` isolation untouched.
func Open(graphRoot, runRoot string) (*Store, error) {
	opts := types.StoreOptions{
		GraphRoot:       graphRoot,
		RunRoot:         runRoot,
		GraphDriverName: "overlay",
		GraphDriverOptions: []string{
			"overlay.use_composefs=false",
		},
	}
	// The store's owning identity is inferred from the graphroot itself: this
	// daemon runs as root against stores that belong to unprivileged builders,
	// and everything it creates on their behalf has to end up owned by them
	// (see internal/ownership).
	owner, err := ownership.Of(graphRoot)
	if err != nil {
		return nil, fmt.Errorf("determining owner of graphroot %q: %w", graphRoot, err)
	}

	s, err := storage.GetStore(opts)
	if err != nil {
		return nil, fmt.Errorf("opening store at %q: %w", graphRoot, err)
	}
	return &Store{Store: s, graphRoot: graphRoot, owner: owner}, nil
}

// Close releases this store handle. It does NOT unmount layers or tear down
// the driver, so concurrent podman activity in the same store is unaffected.
func (s *Store) Close() {
	s.Free()
}

// GraphRoot returns the graphroot path this store was opened at.
func (s *Store) GraphRoot() string { return s.graphRoot }

// Owner returns the identity that owns this store, inferred from the graphroot
// directory. Everything this tool creates in the store must end up owned by it.
func (s *Store) Owner() ownership.Owner { return s.owner }

// shiftFrom returns the ownership translation to apply to layer content cloned
// out of src into this store (see ownership.Shift).
func (s *Store) shiftFrom(src *Store) ownership.Shift {
	return ownership.Shift{From: src.owner, To: s.owner}
}

// driverDir is <graphroot>/overlay: layer content plus driver bookkeeping.
func (s *Store) driverDir() string { return filepath.Join(s.graphRoot, driverSubdir) }

// layersDir is <graphroot>/overlay-layers: layer metadata and tar-splits.
func (s *Store) layersDir() string { return filepath.Join(s.graphRoot, layersSubdir) }

// imagesDir is <graphroot>/overlay-images: image metadata and big-data.
func (s *Store) imagesDir() string { return filepath.Join(s.graphRoot, imagesSubdir) }

// linkDir is <graphroot>/overlay/l, the driver's symlink farm that keeps mount
// option strings short.
func (s *Store) linkDir() string { return filepath.Join(s.driverDir(), linkSubdir) }

// layerDir is a layer's driver directory: <graphroot>/overlay/<id>, holding
// diff/, link, lower and (when mounted) merged/ and work/.
func (s *Store) layerDir(layerID string) string { return filepath.Join(s.driverDir(), layerID) }

// layerMetadataDir is a layer's big-data directory under overlay-layers/<id>.
// It does not exist for every layer.
func (s *Store) layerMetadataDir(layerID string) string {
	return filepath.Join(s.layersDir(), layerID)
}

// imageDir is an image's metadata directory: <graphroot>/overlay-images/<id>,
// holding its manifest, config and other big-data items.
func (s *Store) imageDir(imageID string) string { return filepath.Join(s.imagesDir(), imageID) }

// layerDiffDir returns the on-disk directory holding a layer's own content
// (its "diff") for the overlay driver: <graphroot>/overlay/<id>/diff. This is
// the tree we reflink. The layout is verified to exist before use rather than
// assumed (composefs would change it; we disable composefs above).
func (s *Store) layerDiffDir(layerID string) string {
	return filepath.Join(s.layerDir(layerID), "diff")
}

// layerTarSplitPath returns the on-disk path of a layer's gzip-compressed
// tar-split metadata (<graphroot>/overlay-layers/<id>.tar-split.gz). tar-split
// lets the library reassemble the exact original tar stream, which is what
// makes a layer's recorded DiffID reproducible. The file may not exist for
// every layer.
func (s *Store) layerTarSplitPath(layerID string) string {
	return filepath.Join(s.layersDir(), layerID+".tar-split.gz")
}
