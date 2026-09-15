// Package ownership models the unprivileged identity that owns a rootless
// containers/storage store, and the identity shift between two such stores.
//
// It exists because replicanter runs as real root while every store it touches
// belongs to an unprivileged builder. That causes two distinct problems:
//
//  1. Everything the storage library creates while we drive it (layers.json,
//     images.json, per-layer and per-image metadata) lands as root:root. Those
//     files are mode 0600, so the store's own user is locked out of its own
//     store. Owner.ClaimIfRoot hands them back.
//
//  2. Layer *content* is reflinked verbatim out of another store, but podman's
//     rootless id mapping maps container uid 0 to the store owner's own uid: a
//     file owned by builder2 in builder2's store means "root inside the
//     container". The same file in builder1's store must be owned by builder1,
//     or it appears as an unmapped, untouchable uid to builder1's containers.
//     Shift translates exactly that one identity and leaves every other id
//     alone — in particular the subuid/subgid range, which pre-flight has
//     already proven identical across all stores, so those ids already mean the
//     same thing on both sides.
//
// The owning identity is never configured; it is inferred by stat'ing the
// graphroot directory itself.
package ownership

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// Owner is the uid/gid pair that owns a store.
type Owner struct {
	UID, GID uint32
}

// Of returns the owner of path. Symlinks are followed: callers pass store
// directories, whose owner is what identifies the store's user.
func Of(path string) (Owner, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return Owner{}, fmt.Errorf("stat %q: %w", path, err)
	}
	return Owner{UID: st.Uid, GID: st.Gid}, nil
}

func (o Owner) String() string { return fmt.Sprintf("%d:%d", o.UID, o.GID) }

// ClaimIfRoot gives path to o if it is currently owned by uid 0 or gid 0 — the
// signature of a file this root process created rather than the store's user.
// Only the root-owned half of the pair is changed, and only when o is not
// itself root. Symlinks are not followed (the overlay driver's link farm is
// made of them, and they are metadata we create too).
//
// A path that has since vanished is not an error: the library removes and
// recreates its metadata files underneath us, and podman may be pruning
// concurrently. Reports whether ownership was changed.
func (o Owner) ClaimIfRoot(path string) (bool, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, fmt.Errorf("lstat %q: %w", path, err)
	}

	uid, gid := -1, -1 // -1 leaves that half untouched
	if st.Uid == 0 && o.UID != 0 {
		uid = int(o.UID)
	}
	if st.Gid == 0 && o.GID != 0 {
		gid = int(o.GID)
	}
	if uid < 0 && gid < 0 {
		return false, nil
	}

	if err := unix.Lchown(path, uid, gid); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, fmt.Errorf("lchown %q to %s: %w", path, o, err)
	}
	return true, nil
}

// Shift translates the source store owner's identity into the destination
// store owner's. Every other id is passed through unchanged.
type Shift struct {
	From, To Owner
}

// IsIdentity reports whether the shift is a no-op (both stores have the same
// owner), in which case content can be cloned verbatim.
func (s Shift) IsIdentity() bool { return s.From == s.To }

func (s Shift) String() string { return fmt.Sprintf("%s->%s", s.From, s.To) }

// Map returns the ownership that a file carrying (uid, gid) in the source store
// must carry in the destination store.
func (s Shift) Map(uid, gid uint32) (uint32, uint32) {
	if uid == s.From.UID {
		uid = s.To.UID
	}
	if gid == s.From.GID {
		gid = s.To.GID
	}
	return uid, gid
}
