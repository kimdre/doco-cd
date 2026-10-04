package prometheus

import (
	"os"
	"testing"

	clientPrometheus "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestMirrorStatsTracker_CombinesMirrorsAndForgetsRemovedOnes(t *testing.T) {
	t.Parallel()

	packsGauge := clientPrometheus.NewGaugeVec(clientPrometheus.GaugeOpts{Name: "test_git_mirror_packs"}, []string{"repository"})
	sizeGauge := clientPrometheus.NewGaugeVec(clientPrometheus.GaugeOpts{Name: "test_git_mirror_size_bytes"}, []string{"repository"})
	tracker := newMirrorStatsTracker(packsGauge, sizeGauge)

	const (
		repository = "example.com/owner/repo"
		other      = "example.com/owner/other"
	)

	deployedMirror := t.TempDir()
	includedMirror := t.TempDir()
	otherMirror := t.TempDir()

	assertStats := func(repo string, wantPacks, wantSize float64) {
		t.Helper()

		if got := testutil.ToFloat64(packsGauge.WithLabelValues(repo)); got != wantPacks {
			t.Fatalf("packs of %s = %v, want %v", repo, got, wantPacks)
		}

		if got := testutil.ToFloat64(sizeGauge.WithLabelValues(repo)); got != wantSize {
			t.Fatalf("size of %s = %v, want %v", repo, got, wantSize)
		}
	}

	tracker.observe(repository, deployedMirror, 3, 10)
	tracker.observe(repository, includedMirror, 1, 20)
	assertStats(repository, 3, 30)

	// A mirror's new state replaces its previous one instead of adding to it.
	tracker.observe(repository, deployedMirror, 2, 15)
	assertStats(repository, 2, 35)

	// A mirror whose size could not be measured keeps its last measured size.
	tracker.observe(repository, includedMirror, 4, -1)
	assertStats(repository, 4, 35)

	if err := os.RemoveAll(includedMirror); err != nil {
		t.Fatalf("remove mirror: %v", err)
	}

	// A report of any mirror forgets the removed ones.
	tracker.observe(other, otherMirror, 1, 5)
	assertStats(repository, 2, 15)
	assertStats(other, 1, 5)

	if err := os.RemoveAll(deployedMirror); err != nil {
		t.Fatalf("remove mirror: %v", err)
	}

	tracker.observe(other, otherMirror, 1, 5)

	// The repository without any mirror left has no series anymore.
	for name, gauge := range map[string]*clientPrometheus.GaugeVec{"packs": packsGauge, "size": sizeGauge} {
		if got := testutil.CollectAndCount(gauge); got != 1 {
			t.Fatalf("%s series = %d, want only the one of %s", name, got, other)
		}
	}

	assertStats(other, 1, 5)
}

func TestMirrorStatsTracker_OmitsUnmeasuredSize(t *testing.T) {
	t.Parallel()

	packsGauge := clientPrometheus.NewGaugeVec(clientPrometheus.GaugeOpts{Name: "test_git_mirror_packs"}, []string{"repository"})
	sizeGauge := clientPrometheus.NewGaugeVec(clientPrometheus.GaugeOpts{Name: "test_git_mirror_size_bytes"}, []string{"repository"})
	tracker := newMirrorStatsTracker(packsGauge, sizeGauge)

	tracker.observe("example.com/owner/repo", t.TempDir(), 2, -1)

	if got := testutil.ToFloat64(packsGauge.WithLabelValues("example.com/owner/repo")); got != 2 {
		t.Fatalf("packs = %v, want 2", got)
	}

	if got := testutil.CollectAndCount(sizeGauge); got != 0 {
		t.Fatalf("size series = %d, want none without a measured size", got)
	}
}
