package store

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/kimdre/doco-cd/internal/common/types/set"
)

// GCOptions configures artifact garbage collection for one store's base
// directory (see Sweep). It mirrors Flux's artifact-retention model:
// RetentionRecords always wins over RetentionTTL, so a repository that is
// simply quiet for a long time never loses its most recent artifacts.
type GCOptions struct {
	// RetentionRecords is the number of most-recently-modified unreferenced
	// artifacts that are always kept, regardless of RetentionTTL.
	// Mirrors Flux's --artifact-retention-records.
	RetentionRecords int

	// RetentionTTL is how long an unreferenced artifact beyond
	// RetentionRecords is kept before Sweep is allowed to remove it.
	// Mirrors Flux's --artifact-retention-ttl.
	RetentionTTL time.Duration
}

// GCResult reports what Sweep did for one base directory.
type GCResult struct {
	// Kept lists every artifact Sweep decided not to remove: referenced
	// (live), within RetentionRecords, or younger than RetentionTTL.
	Kept []Artifact

	// Removed lists every artifact Sweep deleted.
	Removed []Artifact
}

// Sweep removes unreferenced, expired artifacts under baseDir.
//
// live is the hard safety check: any revision present in it - regardless of
// how it got there - is always kept, no matter its age or position in the
// retention ranking. Callers are expected to build live from every revision
// currently referenced by a deployed stack (across every configured Docker
// context) plus every revision currently being prepared for a deployment
// that has not finished yet (see the source package's in-flight tracker);
// Sweep itself has no way to tell the two apart and does not need to.
//
// Artifacts not in live are ranked by their directory's modification time,
// newest first. The newest RetentionRecords of them are always kept; the
// rest are removed once older than RetentionTTL, measured against now (an
// explicit clock so tests are deterministic).
//
// Sweep never touches anything outside "<baseDir>/artifacts/<revision>"
// (via listArtifacts) - the mirror clone, its lock file, the submodule
// cache, and any in-progress temporary publish directory are never listed
// as candidates in the first place, so they can never be removed here.
//
// A single artifact that fails to stat or remove is recorded as kept and
// does not stop the sweep of the others; all such errors are joined and
// returned alongside the (still valid) result.
func Sweep(baseDir string, live set.Set[Revision], opts GCOptions, now time.Time) (GCResult, error) {
	artifacts, err := listArtifacts(baseDir)
	if err != nil {
		return GCResult{}, fmt.Errorf("sweep %s: %w", baseDir, err)
	}

	type candidate struct {
		Artifact
		modTime time.Time
	}

	var (
		result     GCResult
		candidates []candidate
		errs       []error
	)

	for _, a := range artifacts {
		if live.Contains(a.Revision) {
			result.Kept = append(result.Kept, a)
			continue
		}

		info, statErr := os.Stat(a.Path)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				// Removed by a concurrent sweeper or store in the meantime;
				// nothing left to report either way.
				continue
			}

			errs = append(errs, fmt.Errorf("stat artifact %s: %w", a.Revision, statErr))
			result.Kept = append(result.Kept, a)

			continue
		}

		candidates = append(candidates, candidate{Artifact: a, modTime: info.ModTime()})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})

	cutoff := now.Add(-opts.RetentionTTL)

	for i, c := range candidates {
		if i < opts.RetentionRecords || c.modTime.After(cutoff) {
			result.Kept = append(result.Kept, c.Artifact)
			continue
		}

		if err := os.RemoveAll(c.Path); err != nil {
			errs = append(errs, fmt.Errorf("remove artifact %s: %w", c.Revision, err))
			result.Kept = append(result.Kept, c.Artifact)

			continue
		}

		result.Removed = append(result.Removed, c.Artifact)
	}

	return result, errors.Join(errs...)
}
