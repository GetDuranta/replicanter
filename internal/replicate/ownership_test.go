package replicate

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/getduranta/replicanter/internal/ownership"
	"golang.org/x/sys/unix"
)

const testGraphRoot = "/cache/builders/storage1"

func TestWalkMetadata(t *testing.T) {
	const layerID = "81a37ff7f49f66ca84e3637077bc51d89fab52a975dabe746ce28e4589fa5703"

	tests := []struct {
		dir  string
		want bool
		why  string
	}{
		{testGraphRoot, true, "the graphroot itself"},
		{testGraphRoot + "/overlay", true, "driver dir"},
		{testGraphRoot + "/overlay/" + layerID, true, "a layer's driver dir is bookkeeping"},
		{testGraphRoot + "/overlay/l", true, "the link farm is metadata we create"},
		{testGraphRoot + "/overlay-images", true, "image metadata"},
		{testGraphRoot + "/overlay-images/" + layerID, true, "an image's big-data"},
		{testGraphRoot + "/overlay-layers", true, "layer metadata"},
		{testGraphRoot + "/overlay-layers/" + layerID, true, "a layer's big-data"},

		{testGraphRoot + "/overlay/" + layerID + "/diff", false, "layer content: in-layer ids, not ours"},
		{testGraphRoot + "/overlay/" + layerID + "/merged", false, "a live overlay mount point"},
		{testGraphRoot + "/overlay/" + layerID + "/work", false, "nothing below a layer dir is walked"},
		{testGraphRoot + "/overlay/staging", false, "layer content in flight"},
		{testGraphRoot + "/overlay/tempdirs", false, "layer content pending deletion"},
		{testGraphRoot + "/volumes", false, "libpod's own state, never written by us"},
		{testGraphRoot + "/overlay-containers", false, "libpod's own state"},
		{testGraphRoot + "/networks", false, "libpod's own state"},
		{"/somewhere/else", false, "outside the graphroot entirely"},
	}

	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			if got := walkMetadata(testGraphRoot, tt.dir); got != tt.want {
				t.Fatalf("walkMetadata(%q) = %v, want %v (%s)", tt.dir, got, tt.want, tt.why)
			}
		})
	}
}

// The store-wide metadata files are the ones the library rewrites on every
// save, so they revert to root:root after every mutation. Missing one of them
// means locking the store's user out of its own store.
func TestStoreMetadataPaths(t *testing.T) {
	s := &Store{graphRoot: testGraphRoot}
	got := s.storeMetadataPaths()

	for _, want := range []string{
		testGraphRoot + "/overlay-images/images.json",
		testGraphRoot + "/overlay-layers/layers.json",
		testGraphRoot + "/overlay-images/volatile-images.json",
		testGraphRoot + "/overlay-layers/volatile-layers.json",
		testGraphRoot + "/overlay/l",
		testGraphRoot + "/overlay/staging",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("storeMetadataPaths() is missing %q; got %v", want, got)
		}
	}
}

func TestClaimDirShallowDoesNotDescend(t *testing.T) {
	// claimDirShallow must not walk into a layer's diff/ tree: that content can
	// be enormous, and its ownership belongs to the layer, not to the store.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "diff", "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "link"), []byte("ABCDEF\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Nothing here is root-owned, so this is a no-op that must still succeed.
	if err := claimDirShallow(ownership.Owner{UID: 1001, GID: 1001}, dir); err != nil {
		t.Fatalf("claimDirShallow: %v", err)
	}
	// A layer directory that has already been removed is not an error either.
	if err := claimDirShallow(ownership.Owner{UID: 1001, GID: 1001}, filepath.Join(dir, "gone")); err != nil {
		t.Fatalf("claimDirShallow on a missing dir: %v", err)
	}
}

func TestClaimTreeToleratesMissingRoot(t *testing.T) {
	// Not every layer has a big-data directory.
	if err := claimTree(ownership.Owner{UID: 1001, GID: 1001}, filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("claimTree on a missing root: %v", err)
	}
}

// TestReclaimStoreSweep exercises the full sweep against a real (synthetic)
// store layout. It needs root, because the whole point is reclaiming files that
// only root could have created; it skips otherwise.
func TestReclaimStoreSweep(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to create root-owned files")
	}
	const (
		ownerUID = 65534 // the graphroot's owner: who everything must end up as
		inLayer  = 589824
		layerID  = "5cbf0fe85f6b7238c351ab914a6776f398ca63d4e80414711bce9e86a555f507"
	)

	graphRoot := filepath.Join(t.TempDir(), "storage")
	mkdir := func(parts ...string) string {
		p := filepath.Join(append([]string{graphRoot}, parts...)...)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write := func(p string) string {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	chown := func(p string, uid int) string {
		if err := os.Lchown(p, uid, 0); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Metadata the library creates as root and that must come back to the owner.
	imagesJSON := write(filepath.Join(mkdir(imagesSubdir), "images.json"))
	layersJSON := write(filepath.Join(mkdir(layersSubdir), "layers.json"))
	manifest := write(filepath.Join(mkdir(imagesSubdir, "someimage"), "manifest"))
	tarSplit := write(filepath.Join(graphRoot, layersSubdir, layerID+".tar-split.gz"))
	layerDir := mkdir(driverSubdir, layerID)
	lowerFile := write(filepath.Join(layerDir, "lower"))
	linkFarm := mkdir(driverSubdir, linkSubdir)
	linkEntry := filepath.Join(linkFarm, "ABCDEFG")
	if err := os.Symlink("../"+layerID+"/diff", linkEntry); err != nil {
		t.Fatal(err)
	}

	// Layer content: never touched, whoever owns it. A root-owned file under
	// diff/ would be in-layer ownership, not ours to rewrite.
	diffDir := mkdir(driverSubdir, layerID, "diff", "usr")
	inLayerFile := chown(write(filepath.Join(diffDir, "binary")), inLayer)
	rootInLayerFile := write(filepath.Join(diffDir, "owned-by-in-layer-root"))
	staged := write(filepath.Join(mkdir(driverSubdir, "staging", "tmp1"), "content"))

	// libpod's own state, outside everything this tool writes.
	volume := write(filepath.Join(mkdir("volumes", "vol1"), "data"))

	if err := os.Lchown(graphRoot, ownerUID, ownerUID); err != nil {
		t.Fatal(err)
	}
	s := &Store{graphRoot: graphRoot, owner: ownership.Owner{UID: ownerUID, GID: ownerUID}}
	if err := reclaimStore(s, discardLogger()); err != nil {
		t.Fatalf("reclaimStore: %v", err)
	}

	ownerOf := func(p string) int {
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			t.Fatal(err)
		}
		return int(st.Uid)
	}
	for _, p := range []string{imagesJSON, layersJSON, manifest, tarSplit, layerDir, lowerFile, linkFarm, linkEntry} {
		if got := ownerOf(p); got != ownerUID {
			t.Errorf("%s is owned by %d, want the store owner %d", p, got, ownerUID)
		}
	}
	for _, p := range []string{inLayerFile, rootInLayerFile, staged, volume} {
		want := 0
		if p == inLayerFile {
			want = inLayer
		}
		if got := ownerOf(p); got != want {
			t.Errorf("%s is owned by %d, want %d left untouched", p, got, want)
		}
	}
}
