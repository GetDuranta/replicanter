# replicanter

A root daemon that replicates container image layers between rootless
`containers/storage` stores on the **same btrfs volume** using copy-on-write
**reflinks** instead of serializing/deserializing tar streams. It is an
`rsync`-for-layers: the bytes move via `FICLONE`, the metadata and locking go
through the official `go.podman.io/storage` library so the tool is a safe
second client.

See [PLAN.md](PLAN.md) for the full design, constraints, and invariants.

## How it works

For each store missing an image that another store has, replicanter:

1. Walks the image's layer chain bottom-up (parents before children).
2. For each missing layer, uses the storage library's **staged-layer / custom
   `Differ`** hook: the overlay driver hands the differ a staging directory
   under the destination graphroot (same btrfs → reflink works), the differ
   `CloneTree`s the source layer's `diff/` into it, and reports the layer's
   precomputed digests + uncompressed size. `ApplyStagedLayer` then commits the
   metadata and atomically renames the content into place under the store lock.
3. Transcribes the layer's `tar-split` so the destination can reproduce the
   exact original tar stream (its recorded `DiffID` stays verifiable / pushable).
4. Creates the image record and copies its big-data (manifest, config, …).

### Why staged-differ and not `PutLayer(nil diff)`

PLAN.md Step 2 sketches `PutLayer` with a nil diff. On `go.podman.io/storage`
v1.62.0 (Podman 5.8.2) that path **cannot record a truthful `UncompressedSize`**
(`LayerOptions` has no field for it), which violates invariant #2. The
staged-`Differ` path is the library's designed hook for placing layer bytes
out-of-band with caller-supplied digests/sizes, and it gives atomic visibility
(invariant #6) for free.

## Build

```
make build      # produces bin/replicanter
make test
```

The build needs the `exclude_graphdriver_btrfs` tag (baked into the Makefile)
to drop the btrfs graph driver's C dependency. This is unrelated to reflinks —
we run on the **overlay** graph driver atop a btrfs filesystem.

Pinned to `go.podman.io/storage@v1.62.0` to match Podman 5.8.2's on-disk store
format (preflight check #3). Bump in lockstep with the deployed Podman.

## Usage

Daemon (watches a trigger file; runs as **root**):

```
sudo replicanter \
  --graphroot /home/builder1/.local/share/containers/storage \
  --graphroot /home/builder2/.local/share/containers/storage \
  --graphroot /home/builder3/.local/share/containers/storage \
  --lock        /cache/builders/replication.lock \
  --trigger-dir /cache/builders \
  --trigger-file replicate.trigger
```

Drop the trigger file to request a run; it is consumed on success:

```
touch /cache/builders/replicate.trigger
```

One-shot (no daemon):

```
sudo replicanter --once --graphroot <a> --graphroot <b> [--verify] [--dry-run]
```

| Flag | Meaning |
|------|---------|
| `--graphroot` (repeatable, ≥2) | Stores to synchronize (union of all images). |
| `--once` | Replicate once and exit. |
| `--dry-run` | Log what would be replicated; mutate nothing. |
| `--verify` | Recompute each reflinked layer's DiffID and assert it matches the source (slow; for CI). |
| `--lock` | Global replication flock path (acquired first). |
| `--runroot-base` | Private, root-owned base for per-store runroots (never a builder's runroot). |
| `--trigger-dir` / `--trigger-file` | Watched folder / file name (daemon mode). |
| `--interval` | Trigger poll interval (daemon mode). |

## Pre-flight checks (all fail loudly)

- Every pair of graphroots is reflink-capable (same btrfs subvolume) — probed
  with a real `FICLONE`, never `--reflink=auto`.
- The owning users of all graphroots share an identical subuid **and** subgid
  range (so subuid-owned in-layer files are mutually controllable).
- Built against a pinned storage version matching the on-disk format.

## Locking

Fixed order, no finer granularity:

1. Global replication flock (`--lock`) — prevents two replicator invocations
   racing. Acquired **first**.
2. Destination store lock — taken internally by the storage library around
   `PrepareStagedLayer` / `ApplyStagedLayer` / `CreateImage`. Acquired **second**.

## Limitations / non-goals

- **composefs is not supported** (stores are opened with it disabled); the
  reflink differ emits a plain directory-format layer. Out of scope per PLAN.md.
- Single-filesystem only — reflink cannot cross hosts. For cross-host sharing a
  registry-backed cache remains the answer.
- Interim measure pending the upstream "shared graphroot, per-UID runroot" fix.
