package stages

import (
	"reflect"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/deploy"
)

func TestMergeDeploymentEnvironment(t *testing.T) {
	t.Parallel()

	config := deploy.New("app", "main")
	config.Environment = map[string]string{
		"SHARED": "configured",
		"EXTRA":  "value",
	}
	config.Internal.Environment = map[string]string{
		"SHARED":      "dotenv",
		"DOTENV_ONLY": "preserved",
	}

	mergeDeploymentEnvironment(config)

	want := map[string]string{
		"SHARED":      "configured",
		"EXTRA":       "value",
		"DOTENV_ONLY": "preserved",
	}
	if !reflect.DeepEqual(config.Internal.Environment, want) {
		t.Fatalf("Internal.Environment = %v, want %v", config.Internal.Environment, want)
	}
}

func TestMergeDeploymentEnvironmentInitializesInternalEnvironment(t *testing.T) {
	t.Parallel()

	config := deploy.New("app", "main")
	config.Environment = map[string]string{"APP_ENV": "production"}

	mergeDeploymentEnvironment(config)

	if got := config.Internal.Environment["APP_ENV"]; got != "production" {
		t.Fatalf("Internal.Environment[APP_ENV] = %q, want %q", got, "production")
	}
}
