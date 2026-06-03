package replicate

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"

	"github.com/opencontainers/go-digest"
	graphdriver "go.podman.io/storage/drivers"
	"go.podman.io/storage/pkg/archive"
)

// reflinkDiffer is a graphdriver.Differ that populates a staged layer by
// reflinking the content of a source layer's diff directory into the staging
// directory the driver provides, instead of unpacking a tar stream. This is
// the short-circuit: layer bytes move via copy-on-write, not serialization.
//
// The digests/sizes/ownership are transcribed verbatim from the source layer
// record so that, once the staged layer is committed, the destination layer's
// metadata is byte-truthful about its content (PLAN.md invariant #2). The
// library copies these straight from the returned DriverWithDifferOutput onto
// the new layer record (layers.go: applyDiffFromStagingDirectory).
type reflinkDiffer struct {
	srcDiffDir         string
	srcTarSplitGzPath  string // "" if the source layer has no tar-split
	uncompressedDigest digest.Digest
	uncompressedSize   int64
	compressedDigest   digest.Digest
	tocDigest          digest.Digest
	uids, gids         []uint32
}

// ApplyDiff is invoked by the overlay driver with dest set to a freshly
// created staging directory that lives under the destination graphroot (hence
// on the same btrfs subvolume as the source — reflink is valid). We clone the
// source layer's content into it and report the precomputed metadata.
//
// The TarOptions (id-mapping for tar extraction) and DifferOptions are
// deliberately ignored: we copy the exact on-disk bytes and ownership of the
// source layer, which is correct precisely because all builders share an
// identical subuid/subgid range (a documented precondition).
func (d *reflinkDiffer) ApplyDiff(dest string, _ *archive.TarOptions, _ *graphdriver.DifferOptions) (graphdriver.DriverWithDifferOutput, error) {
	if err := cloneTreeInto(d.srcDiffDir, dest); err != nil {
		return graphdriver.DriverWithDifferOutput{}, fmt.Errorf("reflinking %q into staging dir: %w", d.srcDiffDir, err)
	}

	out := graphdriver.DriverWithDifferOutput{
		Target:             dest,
		Size:               d.uncompressedSize,
		UncompressedDigest: d.uncompressedDigest,
		CompressedDigest:   d.compressedDigest,
		TOCDigest:          d.tocDigest,
		UIDs:               d.uids,
		GIDs:               d.gids,
	}

	// Transcribe tar-split so the destination layer can reproduce the exact
	// original tar stream (and thus its recorded DiffID). The library expects
	// the *uncompressed* stream and gzips it itself, so we decompress the
	// source's .tar-split.gz here. The library closes out.TarSplit; we unlink
	// the temp file up front so it disappears on close.
	if d.srcTarSplitGzPath != "" {
		ts, err := uncompressedTarSplit(d.srcTarSplitGzPath)
		if err != nil {
			return graphdriver.DriverWithDifferOutput{}, fmt.Errorf("transcribing tar-split: %w", err)
		}
		out.TarSplit = ts
	}

	return out, nil
}

func (d *reflinkDiffer) Close() error { return nil }

// uncompressedTarSplit decompresses a .tar-split.gz into an anonymous temp file
// (unlinked immediately, so it is reclaimed when closed) and returns it
// positioned at the start.
func uncompressedTarSplit(gzPath string) (_ *os.File, retErr error) {
	in, err := os.Open(gzPath)
	if err != nil {
		return nil, err
	}
	defer in.Close()

	gz, err := gzip.NewReader(in)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	tmp, err := os.CreateTemp("", "replicanter-tarsplit-*")
	if err != nil {
		return nil, err
	}
	// Unlink now; the open fd keeps the data alive until the library closes it.
	_ = os.Remove(tmp.Name())
	defer func() {
		if retErr != nil {
			tmp.Close()
		}
	}()

	if _, err := io.Copy(tmp, gz); err != nil {
		return nil, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return tmp, nil
}
