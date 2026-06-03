# PLAN.md — `layer-replicate`: reflink-based Podman layer replication

## Purpose for the implementing agent

You are implementing a small Go utility that replicates container image layers
between two **rootless** `containers/storage` stores on the **same btrfs
volume**, using **btrfs reflinks** to copy layer content (instead of
serializing/deserializing tar streams) while using the official
`github.com/containers/storage` library to mutate metadata and hold locks
correctly.

Think of it as a short-circuited `podman image scp` / an `rsync` for layers:
the bytes move via copy-on-write reflink, the metadata moves via the supported
library API.

Read this whole file before writing code. The reflink is the easy 20%; the
metadata correctness and locking are the real work.

---

## Background / problem context

We run a pool of functionally identical, mutually-trusted rootless build users
(`builder1`, `builder2`, … — self-hosted CI runners on a single host). They all
share an identical subuid/subgid range. The expensive, time-consuming step in
each workflow is **building** cache-layer images (e.g. copy `package.json`, run
`npm i`, then reuse that layer for later actions). These built layers are
identical across builders and consume significant disk.

We want each builder's freshly built layers to become available to the other
builders, while deduplicating the data on disk — without paying the cost of
tar serialization/deserialization that `podman save | podman load` or
`podman image scp` incur.

### Why not a shared graphroot

A single shared read-write graphroot was rejected because libpod pins a single
runroot/tmp path in the store DB (`DBConfig` in `db.sql`), so a second UID
opening the shared store resolves the first user's runroot and fails
(`RunRoot ... not writable`, `lstat /run/user/<other-uid>/libpod: permission
denied`). An upstream issue has been filed requesting "shared graphroot,
per-UID runroot." Until that lands, each builder keeps its **own private store**
and we **replicate** layers between them.

### Why not a local registry

A registry-backed `--cache-to/--cache-from` works but pays a tar-over-socket
round trip even though the layers already exist on the same disk. We want to
avoid that serialization entirely.

### Environment (confirm at runtime; do not hardcode)

- Fedora 44, btrfs root volume, current Podman / `containers/storage`.
- overlay driver. `use_composefs = "true"` is in use.
- Identical `/etc/subuid` + `/etc/subgid` ranges across all builder users.
- Each builder: own `graphroot`, own private `runroot` under `/run/user/<uid>`.

---

## Hard constraints and invariants

1. **Same btrfs subvolume.** Reflink (`cp --reflink=always` / `FICLONE`) only
   works within one btrfs subvolume. If source and destination stores are on
   different subvolumes, reflink is refused and the premise collapses. The tool
   MUST verify this up front and fail loudly. Never fall back to a full copy
   silently — never use `--reflink=auto`.

2. **Metadata must not lie.** A layer records `UncompressedDigest`,
   `UncompressedSize`, and (where applicable) `CompressedDigest`. If we reflink
   content behind the library's back, we MUST transcribe these values from the
   source layer so they remain truthful. A layer whose recorded digest disagrees
   with its on-disk content is corruption and will break composefs/fs-verity
   verification and digest-based dedup.

3. **The library owns metadata + locking.** All metadata mutation goes through
   `containers/storage` API calls (`PutLayer`/`CreateImage`/big-data setters),
   never by editing `layers.json` / `images.json` / `db.sql` directly. This is
   what makes the tool a safe second client.

4. **Layers are replicated bottom-up.** A child layer cannot be created before
   its parent exists in the destination. Walk the parent chain and process
   parents first.

5. **Idempotent.** If the destination already has a layer (by ID), skip its
   content copy — but still ensure the image-level reference exists so refcounts
   are correct. Re-running replication must be cheap and safe.

6. **Atomic visibility.** A layer must never be observable by destination podman
   in a half-populated state. Stage content, then commit metadata, then make
   visible (rename/commit under lock).

---

## Locking model

Two locks, fixed acquisition order, no finer granularity:

1. **Global replication lock** (advisory flock on a well-known path, e.g.
   `/cache/builders/replication.lock`). Prevents two replicator invocations from
   racing each other. Acquire FIRST.

2. **Destination store lock** (provided by the `containers/storage` library —
   `PutLayer`/`CreateImage` take it internally; bracket the reflink step so the
   content copy is covered too). Acquire SECOND.

Order is always replication-lock → store-lock, never the reverse → no deadlock.
Do not implement per-layer lock ordering; it adds deadlock surface for no real
contention benefit.

## Reflink hook — where the short-circuit lives

`PutLayer` normally takes a `diff io.Reader` (a tar stream) and unpacks it; that
is the serialization we are eliminating. Instead:

- Call the library to create the layer **metadata + directory skeleton** with a
  **nil/empty diff**, while **explicitly setting** the digests/sizes copied from
  the source layer (see invariant #2).
- Reflink the source layer's content into the destination layer's content
  directory yourself (`FICLONE` ioctl preferred; `cp --reflink=always` as a
  prototype shortcut).
- Commit/rename so the layer becomes visible only when complete.

---

## Implementation steps (build in this order)

### Step 1

- A root daemon that watches for trigger file in a well-known folder
- Runs the replication when this file is present

### Step 2 — Plain-path prototype
- `storage.GetStore(...)` for source and destination.
- Resolve image → ordered layer chain (walk `Parent`, reverse to bottom-up).
- For each layer: if present in dst, skip; else `PutLayer` with nil diff +
  explicit digest/size options, then `cp --reflink=always` the `diff/` tree.
- `CreateImage` in dst with the top layer; copy image big-data (manifest,
  config, signatures).
- **Acceptance:** dst `podman` runs the replicated image; `podman image prune`
  in dst does NOT delete it (refcount correct).

### Step 3 — Digest integrity
- Transcribe `UncompressedDigest` / `UncompressedSize` / `CompressedDigest` from
  source layer into the dst layer options.
- Add a `--verify` mode that recomputes the digest of reflinked content and
  asserts equality with the source's recorded digest. Gate it off in prod for
  speed once trusted; run it in CI.

### Step 4 — Snapshot + locking
- Take the global replication flock.
- Snapshot the source graphroot; open the snapshot as source.
- Bracket reflink + metadata commit under the destination store lock.
- Ensure half-built layers are never visible (stage → commit → rename).
- Delete the snapshot on exit (defer, even on error).

## Acceptance criteria (overall)

- Replicating an image from builder1 to builder2 makes it runnable in builder2
  with NO tar serialization in the hot path (reflink only).
- Disk usage does NOT double for replicated layers (verify with
  `btrfs filesystem du` / `compsize` that extents are shared).
- `podman image prune` in the destination preserves replicated images
  (refcounts correct); replicated layers are reaped only when truly unreferenced.
- Re-running replication is a fast no-op when nothing changed.
- `podman ps` isolation between builders is preserved (each keeps its private
  runroot; this tool only touches the persistent layer/image store).
- No corruption under concurrent destination podman activity (locking holds).
- ComposeFS is not a priority, no need to support it now

---

## Pre-flight checks the tool must perform

1. Source and destination graphroots are on the **same btrfs subvolume**
   (reflink-capable). `cp --reflink=always` of a probe file must succeed; abort
   loudly otherwise.
2. Subuid/subgid ranges of source and destination users are identical (so
   subuid-owned in-layer files are mutually controllable).
3. The `containers/storage` library version the tool is built against matches
   (or is compatible with) the stores' on-disk format. Pin the dependency to the
   deployed Podman's storage version; the API signatures and on-disk schema
   drift between releases.

---

## Risks / explicit non-goals

- **Do NOT** hand-edit `layers.json`, `images.json`, or `db.sql`. All metadata
  goes through the library so locking and schema correctness are guaranteed.
- **Do NOT** use `--reflink=auto` anywhere; it hides cross-subvolume fallback to
  full copy, silently defeating the purpose.
- **Do NOT** assume the `diff/` directory layout under composefs; verify on the
  real version first.
- This tool is NOT a cross-host solution (reflink is single-filesystem). For
  cross-host sharing, a registry-backed cache remains the answer; that is out of
  scope here.
- This is an interim measure pending the upstream "shared graphroot, per-UID
  runroot" fix. Keep the design simple enough to retire if upstream lands.

---

## Suggested CLI shape

```
layer-replicate \
  --graphroot <graphroot1>
  --graphroot <graphroot2>
  --graphroot <graphroot3>
  [--verify]              # recompute + assert digests \
  [--lock /cache/builders/replication.lock] \
  [--dry-run]
```
