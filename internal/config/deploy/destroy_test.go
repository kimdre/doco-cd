package deploy

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestConfig_DestroyBoolOrObject(t *testing.T) {
	t.Parallel()

	t.Run("yaml bool true uses defaults", func(t *testing.T) {
		t.Parallel()

		filePath := filepath.Join(t.TempDir(), ".doco-cd.yaml")

		err := createTestFile(t, filePath, `name: test
compose_files: ["compose.yaml"]
destroy: true
`)
		if err != nil {
			t.Fatal(err)
		}

		configs, err := GetConfigFromYAML(filePath, true)
		if err != nil {
			t.Fatal(err)
		}

		if !configs[0].Destroy.Enabled {
			t.Fatal("expected destroy.enabled to be true")
		}

		if !configs[0].Destroy.RemoveVolumes || !configs[0].Destroy.RemoveImages || !configs[0].Destroy.RemoveRepoDir {
			t.Fatalf("expected destroy defaults to stay true, got %+v", configs[0].Destroy)
		}
	})

	t.Run("yaml object still works", func(t *testing.T) {
		t.Parallel()

		filePath := filepath.Join(t.TempDir(), ".doco-cd.yaml")

		err := createTestFile(t, filePath, `name: test
compose_files: ["compose.yaml"]
destroy:
  enabled: true
  remove_volumes: false
  remove_images: false
  remove_dir: false
`)
		if err != nil {
			t.Fatal(err)
		}

		configs, err := GetConfigFromYAML(filePath, true)
		if err != nil {
			t.Fatal(err)
		}

		if !configs[0].Destroy.Enabled {
			t.Fatal("expected destroy.enabled to be true")
		}

		if configs[0].Destroy.RemoveVolumes || configs[0].Destroy.RemoveImages || configs[0].Destroy.RemoveRepoDir {
			t.Fatalf("expected destroy object values to be respected, got %+v", configs[0].Destroy)
		}
	})

	t.Run("yaml bool no-default decode only toggles enable", func(t *testing.T) {
		t.Parallel()

		filePath := filepath.Join(t.TempDir(), ".doco-cd.yaml")

		err := createTestFile(t, filePath, `name: test
compose_files: ["compose.yaml"]
destroy: true
`)
		if err != nil {
			t.Fatal(err)
		}

		configs, err := GetConfigFromYAML(filePath, false)
		if err != nil {
			t.Fatal(err)
		}

		if !configs[0].Destroy.Enabled {
			t.Fatal("expected destroy.enabled to be true")
		}

		if configs[0].Destroy.RemoveVolumes || configs[0].Destroy.RemoveImages || configs[0].Destroy.RemoveRepoDir {
			t.Fatalf("expected no-default decode to leave destroy option flags unset, got %+v", configs[0].Destroy)
		}
	})

	t.Run("json bool true uses defaults", func(t *testing.T) {
		t.Parallel()

		var cfg Config
		if err := json.Unmarshal([]byte(`{"name":"test","compose_files":["compose.yaml"],"destroy":true}`), &cfg); err != nil {
			t.Fatal(err)
		}

		if !cfg.Destroy.Enabled {
			t.Fatal("expected destroy.enabled to be true")
		}

		if !cfg.Destroy.RemoveVolumes || !cfg.Destroy.RemoveImages || !cfg.Destroy.RemoveRepoDir {
			t.Fatalf("expected destroy defaults to stay true, got %+v", cfg.Destroy)
		}
	})
}
