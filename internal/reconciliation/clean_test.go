package reconciliation

import "testing"

func TestIsCleanupTargetMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		runConfigTargets map[string]struct{}
		stackTarget      string
		want             bool
	}{
		{
			name:             "legacy mode when no run target is available",
			runConfigTargets: map[string]struct{}{},
			stackTarget:      "nas",
			want:             true,
		},
		{
			name:             "custom target matches same target",
			runConfigTargets: map[string]struct{}{"updater": {}},
			stackTarget:      "updater",
			want:             true,
		},
		{
			name:             "custom target does not match different target",
			runConfigTargets: map[string]struct{}{"updater": {}},
			stackTarget:      "nas",
			want:             false,
		},
		{
			name:             "custom target does not match unlabeled stack",
			runConfigTargets: map[string]struct{}{"updater": {}},
			stackTarget:      "",
			want:             false,
		},
		{
			name:             "default target matches unlabeled stack",
			runConfigTargets: map[string]struct{}{"": {}},
			stackTarget:      "",
			want:             true,
		},
		{
			name:             "default target matches default label",
			runConfigTargets: map[string]struct{}{"": {}},
			stackTarget:      "  ",
			want:             true,
		},
		{
			name:             "default target does not match custom target stack",
			runConfigTargets: map[string]struct{}{"": {}},
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
