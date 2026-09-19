package source

import "sync"

// inFlightTracker reference-counts revisions that a Prepare-to-deploy call is
// currently using, keyed by repository name and revision. It exists to close
// a gap in artifact garbage collection: a revision that Prepare has just
// resolved and published has not yet been deployed, so no container or
// service carries its label yet, and it would look unreferenced to a
// label-based live set alone (see internal/gc). MarkInFlight/IsInFlight
// bracket exactly the window where that gap exists.
//
// The reference-counted map/mutex shape mirrors internal/reconciliation.deploymentTracker.
type inFlightTracker struct {
	mu        sync.Mutex
	revisions map[string]int
}

var inFlight = &inFlightTracker{revisions: make(map[string]int)}

// inFlightKey builds the tracking key for a repository/revision pair.
func inFlightKey(repoName, revision string) string {
	return repoName + "@" + revision
}

// MarkInFlight records that revision of repoName is in active use by a
// Prepare-to-deploy call and returns a function that releases that record.
// Calls are reference-counted: the same repository/revision pair can be
// marked in flight by more than one concurrent call at once, and the record
// is only fully released once every marker has been released.
//
// Callers must defer the returned function immediately, starting right after
// Prepare succeeds and lasting until the deployment it fed either finishes or
// fails - the entire span during which a garbage collector could otherwise
// mistake the freshly published artifact for an unreferenced one.
//
// If repoName or revision is empty, MarkInFlight is a no-op and returns a no-op release function.
func MarkInFlight(repoName, revision string) func() {
	if repoName == "" || revision == "" {
		return func() {}
	}

	key := inFlightKey(repoName, revision)

	inFlight.mu.Lock()
	inFlight.revisions[key]++
	inFlight.mu.Unlock()

	var once sync.Once

	return func() {
		once.Do(func() {
			inFlight.mu.Lock()
			defer inFlight.mu.Unlock()

			if count := inFlight.revisions[key]; count <= 1 {
				delete(inFlight.revisions, key)
			} else {
				inFlight.revisions[key] = count - 1
			}
		})
	}
}

// IsInFlight reports whether revision of repoName is currently marked
// in-flight by an unreleased MarkInFlight call.
func IsInFlight(repoName, revision string) bool {
	if repoName == "" || revision == "" {
		return false
	}

	inFlight.mu.Lock()
	defer inFlight.mu.Unlock()

	return inFlight.revisions[inFlightKey(repoName, revision)] > 0
}
