package main

import "testing"

func TestShouldForceDeploy(t *testing.T) {
	tests := []struct {
		name      string
		stackName string
		commits   []string
		expected  []bool
	}{
		{
			name:      "No loop detected",
			stackName: "stackA",
			commits:   []string{"commit1", "commit2", "commit3"},
			expected:  []bool{false, false, false},
		},
		{
			name:      "Loop detected after 3 same commits",
			stackName: "stackB",
			commits:   []string{"commitX", "commitX", "commitX", "commitY"},
			expected:  []bool{false, false, true, false},
		},
		{
			name:      "Multiple stacks with independent loops",
			stackName: "stackC",
			commits:   []string{"commitA", "commitA", "commitB", "commitB", "commitB"},
			expected:  []bool{false, false, false, false, true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset the tracker for each test case
			deploymentLoopTracker.Lock()
			deploymentLoopTracker.loops = make(map[string]struct {
				lastCommit string
				count      uint
			})
			deploymentLoopTracker.Unlock()

			for i, commit := range tt.commits {
				result := shouldForceDeploy(tt.stackName, commit, 3)
				if result != tt.expected[i] {
					t.Errorf("shouldForceDeploy(%s, %s) = %v; want %v", tt.stackName, commit, result, tt.expected[i])
				}
			}
		})
	}
}
