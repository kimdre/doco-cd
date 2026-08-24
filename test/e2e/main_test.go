//go:build e2e

package e2e

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	code := m.Run()

	teardownSuiteHarnesses()

	os.Exit(code)
}
