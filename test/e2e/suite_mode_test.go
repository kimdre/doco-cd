//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"testing"
)

const suiteModeEnv = "E2E_TEST_MODE"

// suiteIncludesStandalone returns true if the current test suite includes standalone coverage,
// false if it only includes swarm coverage, and an error if the mode is invalid.
func suiteIncludesStandalone(mode string) (bool, error) {
	switch mode {
	case "", "standalone":
		return true, nil
	case "swarm":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be standalone or swarm, got %q", suiteModeEnv, mode)
	}
}

// requireStandaloneSuite skips the test if the current suite does not include standalone coverage.
func requireStandaloneSuite(t *testing.T) {
	t.Helper()

	include, err := suiteIncludesStandalone(os.Getenv(suiteModeEnv))
	if err != nil {
		t.Fatal(err)
	}

	if !include {
		t.Skip("explicit Compose coverage runs in the standalone suite")
	}
}

func TestSuiteIncludesStandalone(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		mode    string
		include bool
		wantErr bool
	}{
		{mode: "", include: true},
		{mode: "standalone", include: true},
		{mode: "swarm"},
		{mode: "invalid", wantErr: true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()

			include, err := suiteIncludesStandalone(tc.mode)
			if (err != nil) != tc.wantErr || include != tc.include {
				t.Fatalf("suiteIncludesStandalone(%q) = %t, %v", tc.mode, include, err)
			}
		})
	}
}
