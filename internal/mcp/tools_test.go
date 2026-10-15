package mcp

import (
	"slices"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/deploy"
)

func TestToolPauseSchema(t *testing.T) {
	t.Parallel()

	schema := mustToolInputSchema[deploy.Config]("pause-test")
	if slices.Contains(schema.Required, "pause") {
		t.Fatal("pause must be optional")
	}

	pause := schema.Properties["pause"]
	if pause == nil || pause.Type != "boolean" || string(pause.Default) != "false" {
		t.Fatalf("pause must be a boolean defaulting to false: %+v", pause)
	}

	resolved, err := pause.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, value := range []bool{true, false} {
		if err := resolved.Validate(value); err != nil {
			t.Errorf("pause schema rejected %#v: %v", value, err)
		}
	}

	for _, value := range []any{
		"true", 1, nil,
		[]any{},
		map[string]any{},
		map[string]any{"deployments": true},
		map[string]any{"reconciliation": true},
	} {
		if err := resolved.Validate(value); err == nil {
			t.Errorf("pause schema accepted invalid input %#v", value)
		}
	}
}
