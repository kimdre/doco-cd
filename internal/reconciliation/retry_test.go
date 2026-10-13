package reconciliation

import (
	"errors"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/stages"
)

func TestSummarizeDeployResultsPreservesRetrySuppression(t *testing.T) {
	t.Parallel()

	suppressed := &stages.RetrySuppressedError{Stacks: []stages.RetrySuppressedStack{{
		Name: "web", Context: "remote", Revision: "failed", Reason: "delay",
		NextRetry: time.Unix(100, 0).UTC(),
	}}}
	failed := errors.New("another stack failed")

	for _, tt := range []struct {
		name    string
		sibling error
		joined  bool
		want    error
	}{
		{name: "alone", want: stages.ErrRetrySuppressed},
		{name: "successful sibling", want: stages.ErrRetrySuppressed},
		{name: "ordinary skip", sibling: stages.ErrSkipDeployment, want: stages.ErrRetrySuppressed},
		{name: "window deferral", sibling: &stages.SyncWindowBlockedError{Stacks: []string{"db"}, Windows: []string{"freeze"}}, want: stages.ErrRetrySuppressed},
		{name: "nested window deferral", sibling: &stages.SyncWindowBlockedError{Stacks: []string{"db"}, Windows: []string{"freeze"}}, joined: true, want: stages.ErrRetrySuppressed},
		{name: "real failure", sibling: failed, want: failed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			results := []stackResult{{name: "web", context: "remote", err: suppressed}}
			if tt.joined {
				results[0].err = errors.Join(suppressed, tt.sibling)
			} else if tt.name != "alone" {
				results = append(results, stackResult{name: "db", err: tt.sibling})
			}

			err := summarizeDeployResults(results, len(results))
			if !errors.Is(err, tt.want) {
				t.Fatalf("result = %v, want %v", err, tt.want)
			}

			if tt.want == failed {
				if errors.Is(err, stages.ErrRetrySuppressed) {
					t.Fatal("real failures must not be classified as suppressed retries")
				}

				return
			}

			got, ok := errors.AsType[*stages.RetrySuppressedError](err)
			if !ok || len(got.Stacks) != 1 || got.Stacks[0].Name != "web" || got.Stacks[0].Context != "remote" {
				t.Fatalf("suppressed target lost: %#v", got)
			}

			if errors.Is(tt.sibling, stages.ErrSyncWindowBlocked) && !errors.Is(err, stages.ErrSyncWindowBlocked) {
				t.Fatal("mixed suppression must retain the sync-window deferral")
			}
		})
	}
}
