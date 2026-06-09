package deploy

import (
	"path/filepath"
	"testing"
)

// Reproduces the server config shape: a multi-document file where only the
// first document carries a hooks block (like .doco-cd.qa-epic-workspace.yaml).
func TestGetConfigFromYAML_MultiDocHooks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, ".doco-cd.qa-epic-workspace.yaml")
	content := `name: qa-epic-chatbotix-backend
repository_url: https://github.com/Truevoice/accentix-doco-cd-config.git
reference: feat/multiple-hooks
working_dir: qa-epic-workspace/backend
compose_files:
  - docker-compose-gcp.yaml
force_recreate: true
hooks:
  on_success:
    - url: https://idp-dev.accentix.dev/api/events
      method: POST
  on_failure:
    - url: https://idp-dev.accentix.dev/api/events
      method: POST
---
name: qa-epic-chatbotix-frontend
repository_url: https://github.com/Truevoice/accentix-doco-cd-config.git
reference: feat/multiple-hooks
working_dir: qa-epic-workspace/frontend
compose_files:
  - docker-compose-gcp.yaml
`

	if err := createTestFile(t, file, content); err != nil {
		t.Fatalf("write file: %v", err)
	}

	configs, err := GetConfigFromYAML(file, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(configs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(configs))
	}

	backend := configs[0]
	t.Logf("backend.Hooks = %+v", backend.Hooks)

	if len(backend.Hooks.OnSuccess) != 1 {
		t.Fatalf("doc1 OnSuccess: expected 1, got %d", len(backend.Hooks.OnSuccess))
	}

	if len(backend.Hooks.OnFailure) != 1 {
		t.Fatalf("doc1 OnFailure: expected 1, got %d", len(backend.Hooks.OnFailure))
	}

	if backend.Hooks.OnSuccess[0].URL != "https://idp-dev.accentix.dev/api/events" {
		t.Fatalf("doc1 hook url wrong: %q", backend.Hooks.OnSuccess[0].URL)
	}

	// doc2 has no hooks
	if len(configs[1].Hooks.OnSuccess) != 0 || len(configs[1].Hooks.OnFailure) != 0 {
		t.Fatalf("doc2 should have no hooks, got %+v", configs[1].Hooks)
	}
}
