package main

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// The healthcheck binary is started every 30 seconds, so it must not pull in
// the dependencies of the doco-cd binary.
func TestDependenciesStaySmall(t *testing.T) {
	t.Parallel()

	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not available")
	}

	out, err := exec.Command(goBin, "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".").Output() // #nosec G204 -- fixed arguments.
	if err != nil {
		t.Fatalf("go list: %v", err)
	}

	allowed := []string{
		"github.com/kimdre/doco-cd/cmd/healthcheck",
		"github.com/kimdre/doco-cd/cmd/doco-cd/healthcheck",
		"github.com/kimdre/doco-cd/internal/common/types/set",
		"github.com/kimdre/doco-cd/internal/logger",
		"github.com/veqryn/slog-dedup",
		"modernc.org/b/v2",
	}

	for dep := range strings.FieldsSeq(string(out)) {
		if !slices.Contains(allowed, dep) {
			t.Errorf("unexpected non-standard dependency %q", dep)
		}
	}
}
