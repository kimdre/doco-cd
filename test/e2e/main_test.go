//go:build e2e

package e2e

import (
	"os"
	"testing"

	tclog "github.com/testcontainers/testcontainers-go/log"
)

type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}

func TestMain(m *testing.M) {
	tclog.SetDefault(discardLogger{})

	code := m.Run()

	teardownSuiteHarnesses()

	os.Exit(code)
}
