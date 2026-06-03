// Package lock provides the global replication flock — the first lock in the
// fixed acquisition order (replication-lock → store-lock) that prevents two
// replicator invocations from racing each other. The destination store lock is
// the second lock and is taken internally by the containers/storage API.
package lock

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Lock is a held advisory file lock. Release it with Unlock.
type Lock struct {
	f *os.File
}

// Acquire takes an exclusive advisory lock (flock) on path, creating the file
// and its parent directory if needed. It blocks until the lock is available.
// This must be acquired BEFORE any destination store lock.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening lock file %q: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock %q: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// TryAcquire is like Acquire but returns (nil, nil) immediately if the lock is
// already held by another process, instead of blocking.
func TryAcquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening lock file %q: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if err == unix.EWOULDBLOCK {
			return nil, nil
		}
		return nil, fmt.Errorf("flock %q: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Unlock releases the lock and closes the underlying file.
func (l *Lock) Unlock() error {
	if l == nil || l.f == nil {
		return nil
	}
	// Closing the fd releases the flock; do it explicitly first for clarity.
	err := unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	cerr := l.f.Close()
	l.f = nil
	if err != nil {
		return err
	}
	return cerr
}
