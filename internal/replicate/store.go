// Package replicate moves container image layers between two
// containers/storage stores on the same btrfs volume by reflinking layer
// content (copy-on-write) instead of serializing/deserializing tar streams,
// while routing all metadata and locking through the official
// go.podman.io/storage API so the tool is a safe second client.
package replicate

import (
	"fmt"
	"path/filepath"

	"go.podman.io/storage"
	"go.podman.io/storage/types"
)

// Store wraps an opened containers/storage store for a single graphroot and
// remembers the graphroot path so we can locate raw layer content on disk for
// reflinking.
type Store struct {
	storage.Store
	graphRoot string
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
	s, err := storage.GetStore(opts)
	if err != nil {
		return nil, fmt.Errorf("opening store at %q: %w", graphRoot, err)
	}
	return &Store{Store: s, graphRoot: graphRoot}, nil
}

// Close releases this store handle. It does NOT unmount layers or tear down
// the driver, so concurrent podman activity in the same store is unaffected.
func (s *Store) Close() {
	s.Free()
}

// GraphRoot returns the graphroot path this store was opened at.
func (s *Store) GraphRoot() string { return s.graphRoot }

// layerDiffDir returns the on-disk directory holding a layer's own content
// (its "diff") for the overlay driver: <graphroot>/overlay/<id>/diff. This is
// the tree we reflink. The layout is verified to exist before use rather than
// assumed (composefs would change it; we disable composefs above).
func (s *Store) layerDiffDir(layerID string) string {
	return filepath.Join(s.graphRoot, "overlay", layerID, "diff")
}

// layerTarSplitPath returns the on-disk path of a layer's gzip-compressed
// tar-split metadata (<graphroot>/overlay-layers/<id>.tar-split.gz). tar-split
// lets the library reassemble the exact original tar stream, which is what
// makes a layer's recorded DiffID reproducible. The file may not exist for
// every layer.
func (s *Store) layerTarSplitPath(layerID string) string {
	return filepath.Join(s.graphRoot, "overlay-layers", layerID+".tar-split.gz")
}
