package reconciliation

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/common/types/set"
)

func TestIsCleanupTargetMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		runConfigTargets set.Set[string]
		stackTarget      string
		want             bool
	}{
		{
			name:             "legacy mode when no run target is available",
			runConfigTargets: set.New[string](),
			stackTarget:      "nas",
			want:             true,
		},
		{
			name:             "custom target matches same target",
			runConfigTargets: set.New("updater"),
			stackTarget:      "updater",
			want:             true,
		},
		{
			name:             "custom target does not match different target",
			runConfigTargets: set.New("updater"),
			stackTarget:      "nas",
			want:             false,
		},
		{
			name:             "custom target does not match unlabeled stack",
			runConfigTargets: set.New("updater"),
			stackTarget:      "",
			want:             false,
		},
		{
			name:             "default target matches unlabeled stack",
			runConfigTargets: set.New(""),
			stackTarget:      "",
			want:             true,
		},
		{
			name:             "default target matches default label",
			runConfigTargets: set.New(""),
			stackTarget:      "  ",
			want:             true,
		},
		{
			name:             "default target does not match custom target stack",
			runConfigTargets: set.New(""),
			stackTarget:      "nas",
			want:             false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := isCleanupTargetMatch(tt.runConfigTargets, tt.stackTarget); got != tt.want {
				t.Fatalf("isCleanupTargetMatch() = %v, want %v", got, tt.want)
			}
		})
	}
}
