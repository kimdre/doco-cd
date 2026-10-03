package stages

import (
	"sync"

	"github.com/kimdre/doco-cd/internal/common/types/set"
)

// RepositoryRemovals collects the repositories whose directory a destroy with
// destroy.remove_dir asked to remove during one deployment job.
//
// The destroy stage cannot remove the directory itself. Other stacks can still
// deploy from the same repository and mount files from its artifacts, and the
// running job holds locks that hide whether they do. The caller of the job
// processes the requests after the job has released its locks (see
// internal/gc.RepositoryRemover). It is safe for concurrent use, because the
// stacks of a job run in parallel.
type RepositoryRemovals struct {
	mu    sync.Mutex
	names set.Set[string]
}

// NewRepositoryRemovals creates an empty RepositoryRemovals.
func NewRepositoryRemovals() *RepositoryRemovals {
	return &RepositoryRemovals{names: set.New[string]()}
}

// Add records a removal request for the repository. Empty names are ignored.
func (r *RepositoryRemovals) Add(repoName string) {
	if r == nil || repoName == "" {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.names.Add(repoName)
}

// Names returns the requested repositories in sorted order.
func (r *RepositoryRemovals) Names() []string {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return set.SortedSlice(r.names)
}
