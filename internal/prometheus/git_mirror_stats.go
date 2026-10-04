package prometheus

import (
	"errors"
	"io/fs"
	"os"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// gitMirrorStats reports the packfiles of every bare mirror of a repository.
// A repository can be mirrored more than once - as a deployed repository, once
// per included reference and once per parent repository that uses it as a
// submodule - and all of these mirrors report under the same repository label,
// so a single gauge value per mirror would overwrite the others.
var gitMirrorStats = newMirrorStatsTracker(GitMirrorPacks, GitMirrorSizeBytes)

// mirrorStats is the last reported state of a mirror.
type mirrorStats struct {
	packs int
	// size is negative while the mirror's size could not be measured.
	size int64
}

// mirrorStatsTracker remembers the last reported state of each mirror and sets
// packs to the highest pack count and size to the combined size of a
// repository's mirrors. The highest pack count, unlike a sum, stays comparable
// to the compaction threshold, which applies to each mirror on its own.
type mirrorStatsTracker struct {
	packs *prometheus.GaugeVec
	size  *prometheus.GaugeVec

	mu sync.Mutex
	// mirrors maps repositories to the last reported state of each of their mirror paths.
	mirrors map[string]map[string]mirrorStats
}

func newMirrorStatsTracker(packs, size *prometheus.GaugeVec) *mirrorStatsTracker {
	return &mirrorStatsTracker{
		packs:   packs,
		size:    size,
		mirrors: make(map[string]map[string]mirrorStats),
	}
}

// observe records the pack count and size of the mirror at path and updates
// the gauges. A negative size keeps the mirror's last measured size.
//
// Mirrors whose directory no longer exists, e.g. because their repository was
// removed, are forgotten on every call, and a repository without any mirror
// left loses its series. A mirror that is only being re-cloned at that moment
// is reported again once the clone finished.
func (t *mirrorStatsTracker) observe(repository, path string, packs int, size int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.mirrors[repository] == nil {
		t.mirrors[repository] = make(map[string]mirrorStats)
	}

	if previous, ok := t.mirrors[repository][path]; ok && size < 0 {
		size = previous.size
	}

	t.mirrors[repository][path] = mirrorStats{packs: packs, size: size}

	for repo, mirrors := range t.mirrors {
		var (
			maxPacks  int
			totalSize int64
			sized     bool
		)

		for mirrorPath, stats := range mirrors {
			if _, err := os.Stat(mirrorPath); errors.Is(err, fs.ErrNotExist) {
				delete(mirrors, mirrorPath)
				continue
			}

			maxPacks = max(maxPacks, stats.packs)

			if stats.size >= 0 {
				totalSize += stats.size
				sized = true
			}
		}

		if len(mirrors) == 0 {
			delete(t.mirrors, repo)
			t.packs.DeleteLabelValues(repo)
			t.size.DeleteLabelValues(repo)

			continue
		}

		t.packs.WithLabelValues(repo).Set(float64(maxPacks))

		if sized {
			t.size.WithLabelValues(repo).Set(float64(totalSize))
		} else {
			t.size.DeleteLabelValues(repo)
		}
	}
}
