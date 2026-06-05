package git

import "testing"

func TestResolveSubmoduleURL(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		parentURL string
		subURL    string
		expected  string
		expectErr bool
	}{
		{
			name:      "resolve relative https sibling",
			parentURL: "https://github.com/example/app.git",
			subURL:    "../periphery.git",
			expected:  "https://github.com/example/periphery.git",
		},
		{
			name:      "resolve relative ssh sibling",
			parentURL: "ssh://git@github.com/example/app.git",
			subURL:    "../periphery.git",
			expected:  "ssh://git@github.com/example/periphery.git",
		},
		{
			name:      "resolve host absolute path",
			parentURL: "https://github.com/example/app.git",
			subURL:    "/other-org/periphery.git",
			expected:  "https://github.com/other-org/periphery.git",
		},
		{
			name:      "keep absolute URL unchanged",
			parentURL: "https://github.com/example/app.git",
			subURL:    "https://github.com/example/periphery.git",
			expected:  "https://github.com/example/periphery.git",
		},
		{
			name:      "error on empty parent URL",
			parentURL: "",
			subURL:    "../periphery.git",
			expectErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resolved, err := resolveSubmoduleURL(tc.parentURL, tc.subURL)
			if tc.expectErr {
				if err == nil {
					t.Fatalf("expected error for parent=%q, submodule=%q", tc.parentURL, tc.subURL)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if resolved != tc.expected {
				t.Fatalf("expected resolved URL %q, got %q", tc.expected, resolved)
			}
		})
	}
}

func TestIsRelativeSubmoduleURL(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		value    string
		expected bool
	}{
		{name: "relative parent", value: "../repo.git", expected: true},
		{name: "relative current", value: "./repo.git", expected: true},
		{name: "host absolute path", value: "/org/repo.git", expected: true},
		{name: "https absolute", value: "https://github.com/org/repo.git", expected: false},
		{name: "https absolute without protocol schema", value: "github.com/org/repo.git", expected: false},
		{name: "ssh absolute", value: "git@github.com:org/repo.git", expected: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := isRelativeSubmoduleURL(tc.value)
			if got != tc.expected {
				t.Fatalf("expected %v for %q, got %v", tc.expected, tc.value, got)
			}
		})
	}
}
