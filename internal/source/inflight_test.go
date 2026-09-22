package source

import (
	"sync"
	"testing"
)

func TestMarkInFlight_TracksUntilReleased(t *testing.T) {
	t.Parallel()

	const repo, revision = "test/mark-in-flight-tracks", "sha1"

	if IsInFlight(repo, revision) {
		t.Fatalf("IsInFlight() = true before MarkInFlight, want false")
	}

	release := MarkInFlight(repo, revision)

	if !IsInFlight(repo, revision) {
		t.Fatalf("IsInFlight() = false after MarkInFlight, want true")
	}

	release()

	if IsInFlight(repo, revision) {
		t.Fatalf("IsInFlight() = true after release, want false")
	}
}

func TestMarkInFlight_RefCounted(t *testing.T) {
	t.Parallel()

	const repo, revision = "test/mark-in-flight-refcounted", "sha1"

	releaseA := MarkInFlight(repo, revision)
	releaseB := MarkInFlight(repo, revision)

	releaseA()

	if !IsInFlight(repo, revision) {
		t.Fatalf("IsInFlight() = false after releasing only one of two markers, want true")
	}

	releaseB()

	if IsInFlight(repo, revision) {
		t.Fatalf("IsInFlight() = true after releasing all markers, want false")
	}
}

func TestMarkInFlight_ReleaseIsIdempotent(t *testing.T) {
	t.Parallel()

	const repo, revision = "test/mark-in-flight-idempotent", "sha1"

	releaseOuter := MarkInFlight(repo, revision)
	releaseInner := MarkInFlight(repo, revision)

	releaseInner()
	releaseInner() // must not underflow the refcount and release the outer marker early

	if !IsInFlight(repo, revision) {
		t.Fatalf("IsInFlight() = false after redundant release, want true (outer marker still held)")
	}

	releaseOuter()

	if IsInFlight(repo, revision) {
		t.Fatalf("IsInFlight() = true after releasing outer marker, want false")
	}
}

func TestMarkInFlight_DistinctRevisionsIndependent(t *testing.T) {
	t.Parallel()

	const repo = "test/mark-in-flight-distinct-revisions"

	release := MarkInFlight(repo, "rev-a")
	defer release()

	if IsInFlight(repo, "rev-b") {
		t.Fatalf("IsInFlight() = true for an unmarked revision of the same repository, want false")
	}
}

func TestMarkInFlight_EmptyArgsAreNoop(t *testing.T) {
	t.Parallel()

	release := MarkInFlight("", "rev")
	release()

	release = MarkInFlight("repo", "")
	release()

	if IsInFlight("", "rev") || IsInFlight("repo", "") {
		t.Fatalf("IsInFlight() = true for an empty repository/revision, want false")
	}
}

func TestMarkInFlight_ConcurrentUse(t *testing.T) {
	t.Parallel()

	const repo, revision = "test/mark-in-flight-concurrent", "sha1"

	var wg sync.WaitGroup

	for range 50 {
		wg.Go(func() {
			release := MarkInFlight(repo, revision)
			defer release()

			_ = IsInFlight(repo, revision)
		})
	}

	wg.Wait()

	if IsInFlight(repo, revision) {
		t.Fatalf("IsInFlight() = true after all concurrent markers released, want false")
	}
}
