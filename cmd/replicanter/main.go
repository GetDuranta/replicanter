// Command replicanter is a root daemon (and one-shot CLI) that replicates
// container image layers between rootless containers/storage stores on the
// same btrfs volume using copy-on-write reflinks instead of tar
// serialization. See PLAN.md for the design and invariants.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/getduranta/replicanter/internal/daemon"
	"github.com/getduranta/replicanter/internal/replicate"
)

// stringSlice collects a repeatable flag (e.g. --graphroot a --graphroot b).
type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	var graphRoots stringSlice
	flag.Var(&graphRoots, "graphroot", "store graphroot to synchronize (repeatable; >=2 required)")
	verify := flag.Bool("verify", false, "recompute and assert each reflinked layer's DiffID (slow; for CI)")
	dryRun := flag.Bool("dry-run", false, "log what would be replicated without mutating any store")
	once := flag.Bool("once", false, "run replication once and exit (no daemon/trigger watching)")
	lockPath := flag.String("lock", "/cache/builders/replication.lock", "global replication lock path")
	runRootBase := flag.String("runroot-base", "/run/replicanter", "private base dir for per-store runroots")
	triggerDir := flag.String("trigger-dir", "/cache/builders", "well-known folder watched for the trigger file")
	triggerFile := flag.String("trigger-file", "replicate.trigger", "trigger file name within --trigger-dir")
	interval := flag.Duration("interval", 5*time.Second, "trigger-file poll interval (daemon mode)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if len(graphRoots) < 2 {
		fmt.Fprintln(os.Stderr, "error: at least two --graphroot values are required")
		flag.Usage()
		os.Exit(2)
	}

	cfg := replicate.RunConfig{
		GraphRoots:  graphRoots,
		RunRootBase: *runRootBase,
		LockPath:    *lockPath,
		Verify:      *verify,
		DryRun:      *dryRun,
		Log:         log,
	}

	if *once {
		if _, err := replicate.RunOnce(cfg); err != nil {
			log.Error("replication failed", "err", err)
			os.Exit(1)
		}
		return
	}

	// Daemon mode: watch the trigger file and run on demand.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err := daemon.Run(ctx, daemon.Config{
		TriggerDir:  *triggerDir,
		TriggerFile: *triggerFile,
		Interval:    *interval,
		Log:         log,
	}, func() error {
		_, err := replicate.RunOnce(cfg)
		return err
	})
	if err != nil {
		log.Error("daemon exited with error", "err", err)
		os.Exit(1)
	}
}
