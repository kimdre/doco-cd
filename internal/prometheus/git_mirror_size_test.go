package prometheus

import (
	"os"
	"testing"

	clientPrometheus "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestMirrorSizeTracker_SumsMirrorsAndForgetsRemovedOnes(t *testing.T) {
	t.Parallel()

	gauge := clientPrometheus.NewGaugeVec(clientPrometheus.GaugeOpts{Name: "test_git_mirror_size_bytes"}, []string{"repository"})
	tracker := newMirrorSizeTracker(gauge)

	const (
		repository = "example.com/owner/repo"
		other      = "example.com/owner/other"
	)

	deployedMirror := t.TempDir()
	includedMirror := t.TempDir()
	otherMirror := t.TempDir()

	assertSize := func(repo string, want float64) {
		t.Helper()

		if got := testutil.ToFloat64(gauge.WithLabelValues(repo)); got != want {
			t.Fatalf("size of %s = %v, want %v", repo, got, want)
		}
	}

	tracker.observe(repository, deployedMirror, 10)
	tracker.observe(repository, includedMirror, 20)
	assertSize(repository, 30)

	// A mirror's new size replaces its previous one instead of adding to it.
	tracker.observe(repository, deployedMirror, 15)
	assertSize(repository, 35)

	if err := os.RemoveAll(includedMirror); err != nil {
		t.Fatalf("remove mirror: %v", err)
	}

	// A report of any mirror forgets the removed ones.
	tracker.observe(other, otherMirror, 5)
	assertSize(repository, 15)
	assertSize(other, 5)

	if err := os.RemoveAll(deployedMirror); err != nil {
		t.Fatalf("remove mirror: %v", err)
	}

	tracker.observe(other, otherMirror, 5)

	// The repository without any mirror left has no series anymore.
	if got := testutil.CollectAndCount(gauge); got != 1 {
		t.Fatalf("series = %d, want only the one of %s", got, other)
	}

	assertSize(other, 5)
}
