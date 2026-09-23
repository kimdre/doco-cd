package docker

import (
	"strings"
	"testing"

	"github.com/docker/cli/cli/compose/template"
)

// TestNormalizeComposeForSwarmSchema_EscapesInterpolation guards the resolved-project
// round trip used for stacks with "include:": the document is fed back into a loader
// that interpolates again, so already-resolved values containing "$" must survive
// verbatim instead of being expanded a second time.
func TestNormalizeComposeForSwarmSchema_EscapesInterpolation(t *testing.T) {
	t.Parallel()

	content := []byte(`name: demo
services:
  app:
    image: nginx
    environment:
      PASS: $2y$10$abcdef
    command:
      - --literal=${NOT_SET}
`)

	normalized, err := normalizeComposeForSwarmSchema(content)
	if err != nil {
		t.Fatalf("normalizeComposeForSwarmSchema() error = %v", err)
	}

	if strings.Contains(string(normalized), "name: demo") {
		t.Errorf("normalized document still contains the unsupported top-level name key:\n%s", normalized)
	}

	for _, want := range []string{"$2y$10$abcdef", "--literal=${NOT_SET}"} {
		restored, err := template.Substitute(extractScalar(t, string(normalized), want), func(string) (string, bool) {
			return "", false
		})
		if err != nil {
			t.Fatalf("Substitute() error = %v", err)
		}

		if restored != want {
			t.Errorf("value after re-interpolation = %q, want %q", restored, want)
		}
	}
}

// extractScalar returns the escaped form of want from the normalized document.
func extractScalar(t *testing.T, doc, want string) string {
	t.Helper()

	escaped := strings.ReplaceAll(want, "$", "$$")
	if !strings.Contains(doc, escaped) {
		t.Fatalf("normalized document does not contain escaped %q:\n%s", want, doc)
	}

	return escaped
}
