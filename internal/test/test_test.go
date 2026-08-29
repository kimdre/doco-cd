package test

import (
	"strings"
	"testing"
)

// TestConvertTestNameIsDeterministicAndCollisionFree pins the properties the
// parallel integration tests rely on: repeated calls for the same test name
// return the same stack name, and long sibling subtest names that share a
// 40-character prefix never collide (a random suffix used to collide roughly
// once in a thousand test pairs and produced flaky Compose name conflicts).
func TestConvertTestNameIsDeterministicAndCollisionFree(t *testing.T) {
	t.Parallel()

	names := []string{
		"TestHandlerData_ProjectApiHandler/Restart_Project_-_Invalid_Timeout",
		"TestHandlerData_ProjectApiHandler/Restart_Project_-_Non-existent_Project_and_Overflowing_Timeout",
		"TestHandlerData_ProjectApiHandler/Restart_Project_-_With_Timeout",
		"short",
	}

	seen := make(map[string]string, len(names))

	for _, name := range names {
		first := ConvertTestName(name)
		if second := ConvertTestName(name); second != first {
			t.Fatalf("ConvertTestName(%q) is not deterministic: %q vs %q", name, first, second)
		}

		if previous, ok := seen[first]; ok {
			t.Fatalf("ConvertTestName collision: %q and %q both map to %q", previous, name, first)
		}

		seen[first] = name

		if strings.ToLower(first) != first || strings.ContainsAny(first, "/ ") {
			t.Fatalf("ConvertTestName(%q) = %q is not a safe stack name", name, first)
		}
	}
}
