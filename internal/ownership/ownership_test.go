package ownership

import (
	"os"
	"path/filepath"
	"testing"
)

func TestShiftMap(t *testing.T) {
	// builder2's store -> builder1's store: both users are in the "builders"
	// group (gid 1001) and share one subuid range starting at 589824.
	shift := Shift{From: Owner{UID: 1002, GID: 1001}, To: Owner{UID: 1001, GID: 1001}}

	tests := []struct {
		name             string
		uid, gid         uint32
		wantUID, wantGID uint32
	}{
		{"container root follows the store owner", 1002, 1001, 1001, 1001},
		{"subuid-mapped id is shared, left alone", 589824, 589824, 589824, 589824},
		{"top of the subid range is left alone", 655359, 655359, 655359, 655359},
		{"destination owner's own id is not remapped", 1001, 1001, 1001, 1001},
		{"an unrelated id is left alone", 1003, 1003, 1003, 1003},
		{"uid and gid shift independently", 1002, 589824, 1001, 589824},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uid, gid := shift.Map(tt.uid, tt.gid)
			if uid != tt.wantUID || gid != tt.wantGID {
				t.Fatalf("Map(%d, %d) = %d, %d; want %d, %d", tt.uid, tt.gid, uid, gid, tt.wantUID, tt.wantGID)
			}
		})
	}
}

func TestShiftIdentity(t *testing.T) {
	var zero Shift
	if !zero.IsIdentity() {
		t.Fatal("the zero Shift must be the identity")
	}
	if uid, gid := zero.Map(1002, 1001); uid != 1002 || gid != 1001 {
		t.Fatalf("the zero Shift changed ownership: %d:%d", uid, gid)
	}
	same := Shift{From: Owner{UID: 7, GID: 8}, To: Owner{UID: 7, GID: 8}}
	if !same.IsIdentity() {
		t.Fatal("a shift between equal owners must be the identity")
	}
}

func TestOf(t *testing.T) {
	dir := t.TempDir()
	owner, err := Of(dir)
	if err != nil {
		t.Fatalf("Of: %v", err)
	}
	if owner.UID != uint32(os.Getuid()) || owner.GID != uint32(os.Getgid()) {
		t.Fatalf("Of(%s) = %s, want %d:%d", dir, owner, os.Getuid(), os.Getgid())
	}
	if _, err := Of(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("Of on a missing path should fail")
	}
}

func TestClaimIfRootLeavesUnprivilegedFilesAlone(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root; this test needs a file not owned by root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The file belongs to the unprivileged test user, not to root, so it is not
	// ours to reclaim — and the attempt must not fail.
	owner := Owner{UID: 12345, GID: 12345}
	changed, err := owner.ClaimIfRoot(path)
	if err != nil {
		t.Fatalf("ClaimIfRoot: %v", err)
	}
	if changed {
		t.Fatal("ClaimIfRoot changed a file that was not root-owned")
	}
}

func TestClaimIfRootToleratesMissingPath(t *testing.T) {
	// The library removes and recreates its metadata files underneath us; a
	// path that has vanished is not an error.
	changed, err := Owner{UID: 1, GID: 1}.ClaimIfRoot(filepath.Join(t.TempDir(), "gone"))
	if err != nil {
		t.Fatalf("ClaimIfRoot on a missing path: %v", err)
	}
	if changed {
		t.Fatal("ClaimIfRoot reported a change for a missing path")
	}
}
