package utils

import "testing"

func TestVerifyAndSanitizePath(t *testing.T) {
	testCases := []struct {
		path        string
		trustedRoot string
		expected    string
		expectError error
	}{
		{
			path:        "/valid/path",
			trustedRoot: "/valid",
			expected:    "/valid/path",
			expectError: nil,
		},
		{
			path:        "/invalid/path",
			trustedRoot: "/valid",
			expected:    "/invalid/path",
			expectError: ErrPathOutsideTrustedRoot,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.path, func(t *testing.T) {
			result, err := VerifyAndSanitizePath(tc.path, tc.trustedRoot)
			if result != tc.expected {
				t.Errorf("expected %s, got %s", tc.expected, result)
			}

			if err != nil && err.Error() != tc.expectError.Error() {
				t.Errorf("expected error %v, got %v", tc.expectError, err)
			}
		})
	}
}
