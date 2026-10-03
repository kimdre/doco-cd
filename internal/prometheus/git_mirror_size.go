package prometheus

import (
	"errors"
	"io/fs"
	"os"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// gitMirrorSizes reports the combined packfile size of every bare mirror of a
// repository. A repository can be mirrored more than once - as a deployed
// repository, once per included reference and once per parent repository that
// uses it as a submodule - and all of these mirrors report under the same
// repository label, so a single gauge value per mirror would overwrite the others.
var gitMirrorSizes = newMirrorSizeTracker(GitMirrorSizeBytes)

// mirrorSizeTracker remembers the last reported size of each mirror and sets
// gauge to the sum of a repository's mirrors.
type mirrorSizeTracker struct {
	gauge *prometheus.GaugeVec

	mu sync.Mutex
	// sizes maps repositories to the last reported size of each of their mirror paths.
	sizes map[string]map[string]int64
}

func newMirrorSizeTracker(gauge *prometheus.GaugeVec) *mirrorSizeTracker {
	return &mirrorSizeTracker{
		gauge: gauge,
		sizes: make(map[string]map[string]int64),
	}
}

// observe records the size of the mirror at path and updates the gauges.
//
// Mirrors whose directory no longer exists, e.g. because their repository was
// removed, are forgotten on every call, and a repository without any mirror
// left loses its series. A mirror that is only being re-cloned at that moment
// is reported again by its next fetch.
func (t *mirrorSizeTracker) observe(repository, path string, size int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.sizes[repository] == nil {
		t.sizes[repository] = make(map[string]int64)
	}

	t.sizes[repository][path] = size

	for repo, mirrors := range t.sizes {
		var total int64

		for mirrorPath, mirrorSize := range mirrors {
			if _, err := os.Stat(mirrorPath); errors.Is(err, fs.ErrNotExist) {
				delete(mirrors, mirrorPath)
				continue
			}

			total += mirrorSize
		}

		if len(mirrors) == 0 {
			delete(t.sizes, repo)
			t.gauge.DeleteLabelValues(repo)

			continue
		}

		t.gauge.WithLabelValues(repo).Set(float64(total))
	}
}
