// Package daemon implements the root daemon that watches a well-known folder
// for a trigger file and runs replication when it appears.
package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Config configures the trigger-file watcher.
type Config struct {
	// TriggerDir is the well-known folder watched for the trigger file.
	TriggerDir string
	// TriggerFile is the file name (within TriggerDir) whose presence requests
	// a replication run. It is consumed (removed) after a successful run.
	TriggerFile string
	// Interval is the polling period.
	Interval time.Duration
	Log      *slog.Logger
}

// Run watches for the trigger file until ctx is cancelled. Each time the file
// is observed it invokes run(); on success the trigger file is removed so the
// next run only happens on a fresh request. run is expected to be idempotent.
//
// Polling (rather than inotify) is deliberate: it needs no extra dependency,
// tolerates the trigger dir not existing yet, and the replication itself is
// gated by the global lock, so a missed/late tick only delays — never corrupts.
func Run(ctx context.Context, cfg Config, run func() error) error {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	triggerPath := filepath.Join(cfg.TriggerDir, cfg.TriggerFile)
	log.Info("daemon watching for trigger", "path", triggerPath, "interval", cfg.Interval)

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("daemon stopping", "reason", ctx.Err())
			return nil
		case <-ticker.C:
			if !triggerPresent(triggerPath) {
				continue
			}
			log.Info("trigger observed; starting replication", "path", triggerPath)
			if err := run(); err != nil {
				// Leave the trigger in place; the interval acts as backoff so
				// we retry next tick rather than dropping the request.
				log.Error("replication run failed; will retry", "err", err)
				continue
			}
			if err := os.Remove(triggerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Error("removing consumed trigger file", "path", triggerPath, "err", err)
			}
		}
	}
}

func triggerPresent(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
