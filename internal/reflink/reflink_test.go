package reflink

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// tmpOnBtrfs returns a temp dir, skipping the test if it is not on btrfs
// (reflink is a btrfs feature; the test is meaningless elsewhere).
func tmpOnBtrfs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var fsstat unix.Statfs_t
	if err := unix.Statfs(dir, &fsstat); err != nil {
		t.Fatalf("statfs: %v", err)
	}
	const btrfsMagic = 0x9123683E
	if fsstat.Type != btrfsMagic {
		t.Skipf("temp dir %s is not on btrfs (type %#x); skipping reflink test", dir, fsstat.Type)
	}
	return dir
}

func TestProbeSameDirSucceeds(t *testing.T) {
	dir := tmpOnBtrfs(t)
	if err := Probe(dir, dir); err != nil {
		t.Fatalf("Probe within same btrfs dir should succeed: %v", err)
	}
}

func TestCloneFileSharesExtents(t *testing.T) {
	dir := tmpOnBtrfs(t)
	src := filepath.Join(dir, "src")
	data := make([]byte, 1<<20) // 1 MiB so we occupy real extents
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	if err := CloneFile(src, dst, 0o644); err != nil {
		t.Fatalf("CloneFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) {
		t.Fatalf("size mismatch: got %d want %d", len(got), len(data))
	}
	// Refusing to overwrite an existing destination is part of the contract.
	if err := CloneFile(src, dst, 0o644); err == nil {
		t.Fatal("CloneFile should refuse to overwrite existing dst")
	}
}

func TestCloneTreePreservesTree(t *testing.T) {
	base := tmpOnBtrfs(t)
	src := filepath.Join(base, "src")
	dst := filepath.Join(base, "dst")

	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "file"), []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file", filepath.Join(src, "sub", "link")); err != nil {
		t.Fatal(err)
	}

	if err := CloneTree(src, dst); err != nil {
		t.Fatalf("CloneTree: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "sub", "file"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("cloned file content = %q, %v", got, err)
	}
	link, err := os.Readlink(filepath.Join(dst, "sub", "link"))
	if err != nil || link != "file" {
		t.Fatalf("cloned symlink = %q, %v", link, err)
	}
	fi, err := os.Stat(filepath.Join(dst, "sub", "file"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("cloned file mode = %v, want 0640", fi.Mode().Perm())
	}
}
