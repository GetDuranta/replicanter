package replicate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/getduranta/replicanter/internal/lock"
	"github.com/getduranta/replicanter/internal/preflight"
)

// RunConfig is the full configuration for one replication run.
type RunConfig struct {
	GraphRoots  []string // two or more store graphroots on the same btrfs subvolume
	RunRootBase string   // private, root-owned base for per-store runroots
	LockPath    string   // global replication flock path
	Verify      bool
	DryRun      bool
	Log         *slog.Logger
}

// RunOnce performs a single end-to-end replication: pre-flight checks, acquire
// the global replication lock, open every store, synchronize, then release
// everything (in reverse order). Safe to call repeatedly; it is idempotent.
func RunOnce(cfg RunConfig) (Stats, error) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}

	if err := preflight.Check(cfg.GraphRoots); err != nil {
		return Stats{}, fmt.Errorf("pre-flight: %w", err)
	}
	log.Info("pre-flight passed", "graphroots", cfg.GraphRoots, "storage_version", preflight.StorageVersion)

	// Lock #1 (global replication lock) — always first in the ordering.
	l, err := lock.Acquire(cfg.LockPath)
	if err != nil {
		return Stats{}, err
	}
	defer func() {
		if err := l.Unlock(); err != nil {
			log.Error("releasing replication lock", "err", err)
		}
	}()

	stores, err := openAll(cfg.GraphRoots, cfg.RunRootBase)
	if err != nil {
		return Stats{}, err
	}
	defer func() {
		for _, s := range stores {
			s.Close()
		}
	}()

	// Lock #2 (destination store lock) is taken internally by the storage API
	// inside Sync (PrepareStagedLayer/ApplyStagedLayer/CreateImage).
	return Sync(stores, Options{Verify: cfg.Verify, DryRun: cfg.DryRun, Log: log})
}

func openAll(graphRoots []string, runRootBase string) ([]*Store, error) {
	stores := make([]*Store, 0, len(graphRoots))
	for _, gr := range graphRoots {
		runRoot := filepath.Join(runRootBase, runRootKey(gr))
		s, err := Open(gr, runRoot)
		if err != nil {
			for _, opened := range stores {
				opened.Close()
			}
			return nil, err
		}
		stores = append(stores, s)
	}
	return stores, nil
}

// runRootKey derives a stable, collision-resistant directory name for a
// graphroot's private runroot.
func runRootKey(graphRoot string) string {
	sum := sha256.Sum256([]byte(graphRoot))
	return hex.EncodeToString(sum[:8])
}
