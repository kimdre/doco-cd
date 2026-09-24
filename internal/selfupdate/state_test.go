package selfupdate

import "testing"

func TestStateHelpers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state              State
		inApplierPhase     bool
		applierFinished    bool
		rolledBackOrFailed bool
		unsuccessful       bool
	}{
		{state: StateStaged},
		{state: StateStarted},
		{state: StateHandover},
		{state: StateDrained},
		{state: StateApplying, inApplierPhase: true},
		{state: StateApplyReady, inApplierPhase: true},
		{state: StateApplyDrained, inApplierPhase: true},
		{state: StateApplied, applierFinished: true},
		{state: StateRolledBack, applierFinished: true, rolledBackOrFailed: true, unsuccessful: true},
		{state: StateFailed, applierFinished: true, rolledBackOrFailed: true, unsuccessful: true},
		{state: StateFinalising, applierFinished: true},
		{state: StateAborted, unsuccessful: true},
	}

	for _, tc := range tests {
		if got := tc.state.InApplierPhase(); got != tc.inApplierPhase {
			t.Errorf("%s.InApplierPhase() = %v, want %v", tc.state, got, tc.inApplierPhase)
		}

		if got := tc.state.ApplierFinished(); got != tc.applierFinished {
			t.Errorf("%s.ApplierFinished() = %v, want %v", tc.state, got, tc.applierFinished)
		}

		if got := tc.state.RolledBackOrFailed(); got != tc.rolledBackOrFailed {
			t.Errorf("%s.RolledBackOrFailed() = %v, want %v", tc.state, got, tc.rolledBackOrFailed)
		}

		if got := tc.state.Unsuccessful(); got != tc.unsuccessful {
			t.Errorf("%s.Unsuccessful() = %v, want %v", tc.state, got, tc.unsuccessful)
		}
	}
}
