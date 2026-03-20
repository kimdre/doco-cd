package filesystem

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestVerifyAndSanitizePath(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		path        string
		trustedRoot string
		expected    string
		expectError error
	}{
		{
			name:        "Valid path",
			path:        "/valid/path",
			trustedRoot: "/valid",
			expected:    "/valid/path",
			expectError: nil,
		},
		{
			name:        "Path outside trusted root",
			path:        "/invalid/path",
			trustedRoot: "/valid",
			expected:    "/invalid/path",
			expectError: ErrPathTraversal,
		},
		{
			name:        "Absolute Path traversal",
			path:        "/valid/../../invalid/path",
			trustedRoot: "/valid",
			expected:    "/valid/../../invalid/path",
			expectError: ErrPathTraversal,
		},
		{
			name:        "Relative Path traversal",
			path:        "../invalid/path",
			trustedRoot: "/valid",
			expected:    "../invalid/path",
			expectError: ErrPathTraversal,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tc.expected, _ = filepath.Abs(tc.expected)
			result, err := VerifyAndSanitizePath(tc.path, tc.trustedRoot)

			if result != tc.expected {
				t.Fatalf("expected %s, got %s", tc.expected, result)
			}

			if err != nil && !errors.Is(err, tc.expectError) {
				t.Fatalf("expected error %v, got %v", tc.expectError, err)
			}
		})
	}
}

func TestInBasePath(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		path        string
		trustedRoot string
		expected    bool
	}{
		{
			name:        "Same path",
			path:        "/valid/path",
			trustedRoot: "/valid/path",
			expected:    true,
		},
		{
			name:        "Path within trusted root",
			path:        "/valid/path",
			trustedRoot: "/valid",
			expected:    true,
		},
		{
			name:        "Path outside trusted root",
			path:        "/invalid/path",
			trustedRoot: "/valid",
			expected:    false,
		},
		{
			name:        "Absolute Path with traversal",
			path:        "/valid/../../invalid/path",
			trustedRoot: "/valid",
			expected:    false,
		},
		{
			name:        "Relative path",
			path:        "valid/path",
			trustedRoot: "valid",
			expected:    true,
		},
		{
			name:        "Relative path with traversal",
			path:        "valid/../../invalid/path",
			trustedRoot: "valid",
			expected:    false,
		},
		{
			name:        "Relative path outside trusted root",
			path:        "invalid/path",
			trustedRoot: "valid",
			expected:    false,
		},
		{
			name:        "Path in base outside trusted root",
			path:        "/base/invalid/path",
			trustedRoot: "/base/valid",
			expected:    false,
		},
		{
			name:        "Path in base in trusted root",
			path:        "/base/valid/path",
			trustedRoot: "/base/valid",
			expected:    true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := InBasePath(tc.trustedRoot, tc.path)

			if result != tc.expected {
				t.Fatalf("expected %v, got %v", tc.expected, result)
			}
		})
	}
}
