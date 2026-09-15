// Package preflight performs the safety checks the replicator must pass before
// touching any store. Every check fails loudly; none silently degrades.
package preflight

import (
	"bufio"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/getduranta/replicanter/internal/ownership"
	"github.com/getduranta/replicanter/internal/reflink"
)

// StorageVersion is the go.podman.io/storage version this binary is built
// against. It must match (or be compatible with) the on-disk store format of
// the deployed Podman. Surfaced in logs so a mismatch is diagnosable.
const StorageVersion = "v1.62.0"

// Check runs every pre-flight check across the given graphroots. It returns
// the first failure; on success the set is safe to replicate within.
func Check(graphRoots []string) error {
	if len(graphRoots) < 2 {
		return fmt.Errorf("need at least two graphroots to replicate, got %d", len(graphRoots))
	}
	if err := checkReflinkable(graphRoots); err != nil {
		return err
	}
	if err := checkSubIDRanges(graphRoots); err != nil {
		return err
	}
	return nil
}

// checkReflinkable verifies every pair of graphroots can share extents via
// reflink (i.e. they live on the same btrfs subvolume). A single failing pair
// aborts: a silent full-copy fallback would defeat the tool's entire premise.
func checkReflinkable(graphRoots []string) error {
	for i := range graphRoots {
		for j := range graphRoots {
			if i == j {
				continue
			}
			if err := reflink.Probe(graphRoots[i], graphRoots[j]); err != nil {
				return fmt.Errorf("graphroots %q and %q are not reflink-capable "+
					"(same btrfs subvolume required): %w", graphRoots[i], graphRoots[j], err)
			}
		}
	}
	return nil
}

// checkSubIDRanges verifies that the owning users of all graphroots share an
// identical subuid AND subgid range. Replicated layers carry subuid-owned
// files verbatim (we reflink raw bytes and ownership); if the ranges differed,
// those files would be owned by the wrong identity in the destination.
func checkSubIDRanges(graphRoots []string) error {
	var firstUser string
	var firstUID, firstGID idRange
	for idx, root := range graphRoots {
		uname, err := ownerUsername(root)
		if err != nil {
			return err
		}
		uidRange, err := lookupSubID("/etc/subuid", uname)
		if err != nil {
			return fmt.Errorf("subuid for %q (graphroot %q): %w", uname, root, err)
		}
		gidRange, err := lookupSubID("/etc/subgid", uname)
		if err != nil {
			return fmt.Errorf("subgid for %q (graphroot %q): %w", uname, root, err)
		}
		if idx == 0 {
			firstUser, firstUID, firstGID = uname, uidRange, gidRange
			continue
		}
		if uidRange != firstUID || gidRange != firstGID {
			return fmt.Errorf("sub-id range mismatch: %q has subuid %s subgid %s, but %q has subuid %s subgid %s",
				firstUser, firstUID, firstGID, uname, uidRange, gidRange)
		}
	}
	return nil
}

type idRange struct {
	start, count uint64
}

func (r idRange) String() string { return fmt.Sprintf("%d:%d", r.start, r.count) }

// ownerUsername returns the username that owns the given path. This is the same
// inference the replicator uses to decide who a store's files must belong to
// (see internal/ownership).
func ownerUsername(path string) (string, error) {
	owner, err := ownership.Of(path)
	if err != nil {
		return "", err
	}
	u, err := user.LookupId(fmt.Sprintf("%d", owner.UID))
	if err != nil {
		return "", fmt.Errorf("looking up owner uid %d of %q: %w", owner.UID, path, err)
	}
	return u.Username, nil
}

// lookupSubID parses /etc/subuid or /etc/subgid for the given user and returns
// their allocated range. Format per line: name:start:count.
func lookupSubID(file, username string) (idRange, error) {
	f, err := os.Open(file)
	if err != nil {
		return idRange{}, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(strings.TrimSpace(sc.Text()), ":")
		if len(fields) != 3 || fields[0] != username {
			continue
		}
		var r idRange
		if _, err := fmt.Sscanf(fields[1]+" "+fields[2], "%d %d", &r.start, &r.count); err != nil {
			return idRange{}, fmt.Errorf("malformed line for %q in %s: %w", username, file, err)
		}
		return r, nil
	}
	if err := sc.Err(); err != nil {
		return idRange{}, err
	}
	return idRange{}, fmt.Errorf("no entry for %q in %s", username, filepath.Base(file))
}
