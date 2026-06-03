// Package reflink performs btrfs copy-on-write clones (FICLONE) of files and
// directory trees, and probes whether two locations can be reflinked at all.
//
// Reflink only works within a single btrfs filesystem (and, depending on the
// kernel, within a single subvolume). We NEVER fall back to a full byte copy:
// a silent fallback would defeat the entire purpose of the tool (sharing
// extents on disk), so every clone either shares extents or fails loudly.
package reflink

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ErrNotReflinkable is returned when the source and destination cannot share
// extents (different filesystems, or cross-subvolume on a kernel that refuses
// it). The caller must treat this as fatal, never as "fall back to copy".
var ErrNotReflinkable = errors.New("paths are not reflink-capable (not the same btrfs subvolume?)")

// CloneFile creates dst as a copy-on-write clone of src using the FICLONE
// ioctl. dst must not already exist; it is created with the given mode. The
// file content shares extents with src until one side is modified.
//
// Only file *content* is cloned. Ownership, xattrs and timestamps are the
// caller's responsibility (see CloneTree, which preserves all of them).
func CloneFile(src, dst string, mode os.FileMode) error {
	srcFd, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFd.Close()

	dstFd, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer dstFd.Close()

	if err := unix.IoctlFileClone(int(dstFd.Fd()), int(srcFd.Fd())); err != nil {
		// Remove the empty/partial destination so we never leave a
		// zero-length file masquerading as cloned content.
		os.Remove(dst)
		if errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("FICLONE %s -> %s: %w", src, dst, ErrNotReflinkable)
		}
		return fmt.Errorf("FICLONE %s -> %s: %w", src, dst, err)
	}
	return nil
}

// Probe verifies that a file created under dstDir can be reflinked from a file
// created under srcDir. Both directories must already exist. It writes and
// removes two small temporary files. A nil return means real reflink works
// between these two locations.
func Probe(srcDir, dstDir string) error {
	srcProbe, err := os.CreateTemp(srcDir, ".replicanter-reflink-src-*")
	if err != nil {
		return fmt.Errorf("creating reflink probe in source %q: %w", srcDir, err)
	}
	defer os.Remove(srcProbe.Name())
	// A reflink of a strictly empty file is a no-op on some kernels, so write
	// a block of data to force a real extent share.
	if _, err := srcProbe.Write(make([]byte, 4096)); err != nil {
		srcProbe.Close()
		return err
	}
	srcProbe.Close()

	dstProbe := filepath.Join(dstDir, ".replicanter-reflink-dst-probe")
	os.Remove(dstProbe)
	defer os.Remove(dstProbe)

	if err := CloneFile(srcProbe.Name(), dstProbe, 0o600); err != nil {
		return err
	}
	return nil
}

// CloneTree recursively clones the directory tree rooted at src into dst,
// reflinking the content of every regular file. Directories, symlinks, devices
// (including overlay whiteout char devices), FIFOs and sockets are recreated;
// ownership (uid/gid), permission bits, and extended attributes are preserved
// exactly so the cloned layer is byte- and metadata-faithful to the source.
//
// dst is created if it does not exist. The caller is expected to run with
// enough privilege (root) to chown files to the subuid-owned identities found
// in container layers.
func CloneTree(src, dst string) error {
	hardlinks := map[uint64]string{} // src inode -> already-created dst path

	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err != nil {
			return fmt.Errorf("lstat %s: %w", path, err)
		}

		switch {
		case d.IsDir():
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case st.Mode&unix.S_IFMT == unix.S_IFLNK:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		case st.Mode&unix.S_IFMT == unix.S_IFREG:
			// Preserve hardlinks: if we've already materialized this inode,
			// link to it instead of cloning a second independent copy.
			if st.Nlink > 1 {
				if existing, ok := hardlinks[st.Ino]; ok {
					os.Remove(target)
					if err := os.Link(existing, target); err != nil {
						return err
					}
					return nil
				}
			}
			if err := CloneFile(path, target, fs.FileMode(st.Mode&0o7777)); err != nil {
				return err
			}
			if st.Nlink > 1 {
				hardlinks[st.Ino] = target
			}
		default:
			// Character/block devices (overlay whiteouts are char 0:0),
			// FIFOs and sockets: recreate the node verbatim.
			os.Remove(target)
			if err := unix.Mknod(target, st.Mode, int(st.Rdev)); err != nil {
				return fmt.Errorf("mknod %s (mode %#o): %w", target, st.Mode, err)
			}
		}

		return applyMetadata(target, &st, path)
	})
}

// applyMetadata copies ownership, permission bits, extended attributes and
// timestamps from a source entry (described by st, located at srcPath) onto
// target, without following symlinks.
func applyMetadata(target string, st *unix.Stat_t, srcPath string) error {
	if err := unix.Lchown(target, int(st.Uid), int(st.Gid)); err != nil {
		return fmt.Errorf("lchown %s to %d:%d: %w", target, st.Uid, st.Gid, err)
	}

	// chmod is meaningless (and errors) on symlinks; their perms are fixed.
	isSymlink := st.Mode&unix.S_IFMT == unix.S_IFLNK
	if !isSymlink {
		if err := unix.Chmod(target, st.Mode&0o7777); err != nil {
			return fmt.Errorf("chmod %s: %w", target, err)
		}
	}

	if err := copyXattrs(srcPath, target); err != nil {
		return err
	}

	// Preserve timestamps last, since chown/chmod/setxattr touch ctime.
	ts := []unix.Timespec{st.Atim, st.Mtim}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, target, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("utimes %s: %w", target, err)
	}
	return nil
}

// copyXattrs transfers every extended attribute from src to dst without
// following symlinks. Overlay relies on xattrs (e.g. trusted.overlay.opaque)
// and security.capability must survive for executables to keep capabilities.
func copyXattrs(src, dst string) error {
	size, err := unix.Llistxattr(src, nil)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) {
			return nil
		}
		return fmt.Errorf("llistxattr %s: %w", src, err)
	}
	if size == 0 {
		return nil
	}
	buf := make([]byte, size)
	size, err = unix.Llistxattr(src, buf)
	if err != nil {
		return fmt.Errorf("llistxattr %s: %w", src, err)
	}

	for _, name := range splitNul(buf[:size]) {
		vsize, err := unix.Lgetxattr(src, name, nil)
		if err != nil {
			return fmt.Errorf("lgetxattr %s %q: %w", src, name, err)
		}
		val := make([]byte, vsize)
		if vsize > 0 {
			if _, err := unix.Lgetxattr(src, name, val); err != nil {
				return fmt.Errorf("lgetxattr %s %q: %w", src, name, err)
			}
		}
		if err := unix.Lsetxattr(dst, name, val, 0); err != nil {
			return fmt.Errorf("lsetxattr %s %q: %w", dst, name, err)
		}
	}
	return nil
}

func splitNul(buf []byte) []string {
	var out []string
	start := 0
	for i, b := range buf {
		if b == 0 {
			if i > start {
				out = append(out, string(buf[start:i]))
			}
			start = i + 1
		}
	}
	return out
}
