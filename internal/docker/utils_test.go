package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/utils"
)

func TestSetConfigHashPrefixes(t *testing.T) {
	ctx := context.Background()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.compose.yaml")

	createComposeFile(t, filePath, composeContents)

	project, err := LoadCompose(ctx, tmpDir, projectName, []string{filePath})
	if err != nil {
		t.Fatal(err)
	}

	var (
		configName    string
		configContent string
		hash          string
	)

	for _, config := range project.Configs {
		configName = config.Name
		configContent = config.Content

		hash, err = generateShortHash(strings.NewReader(configContent))
		if err != nil {
			t.Fatalf("failed to generate hash for config %s: %v", config.Name, err)
		}

		break
	}

	t.Logf("configName: %s", configName)
	t.Logf("configContent: %s", configContent)
	t.Logf("hash: %s", hash)

	err = SetConfigHashPrefixes(project)
	if err != nil {
		t.Fatalf("failed to set config hash prefixes: %v", err)
	}

	serialized, err := json.MarshalIndent(project, "", " ")
	if err != nil {
		t.Error(err.Error())
	}

	t.Log(string(serialized))

	if len(project.Services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(project.Services))
	}

	if len(project.Configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(project.Configs))
	}

	for _, config := range project.Configs {
		if config.Name != configName+"_"+hash {
			t.Errorf("expected config name %s to be %s_%s", config.Name, configName, hash)
		}

		if config.Content != configContent {
			t.Errorf("expected config content to be unchanged, got %s", config.Content)
		}

		for _, service := range project.Services {
			for _, cfg := range service.Configs {
				if cfg.Source == configName {
					if cfg.Source != config.Name {
						t.Errorf("expected service config source to be updated to %s, got %s", config.Name, cfg.Source)
					}
				}
			}
		}
	}
}

func TestSetSecretHashPrefixes(t *testing.T) {
	ctx := context.Background()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.compose.yaml")

	// secrets definition in compose file
	composeContentsWithSecrets := fmt.Sprintf("%s\n%s",
		composeContents, `
secrets:
  secret_example:
    file: ./secret.txt
`)

	secretFilePath := filepath.Join(tmpDir, "secret.txt")

	err := os.WriteFile(secretFilePath, []byte("This is a secret content."), utils.PermOwner)
	if err != nil {
		t.Fatalf("failed to create secret file %s: %v", secretFilePath, err)
	}

	createComposeFile(t, filePath, composeContentsWithSecrets)

	project, err := LoadCompose(ctx, tmpDir, projectName, []string{filePath})
	if err != nil {
		t.Fatal(err)
	}

	var (
		secretName    string
		secretContent string
		hash          string
	)

	for _, secret := range project.Secrets {
		secretName = secret.Name

		contentBytes, err := os.ReadFile(secret.File)
		if err != nil {
			t.Fatalf("failed to read secret file %s: %v", secret.File, err)
		}

		secretContent = string(contentBytes)

		hash, err = generateShortHash(strings.NewReader(secretContent))
		if err != nil {
			t.Fatalf("failed to generate hash for secret %s: %v", secret.Name, err)
		}

		break
	}

	t.Logf("secretName: %s", secretName)
	t.Logf("secretContent: %s", secretContent)
	t.Logf("hash: %s", hash)

	err = SetSecretHashPrefixes(project)
	if err != nil {
		t.Fatalf("failed to set secret hash prefixes: %v", err)
	}

	serialized, err := json.MarshalIndent(project, "", " ")
	if err != nil {
		t.Error(err.Error())
	}

	t.Log(string(serialized))

	if len(project.Services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(project.Services))
	}

	if len(project.Secrets) != 1 {
		t.Fatalf("expected 1 secret, got %d", len(project.Secrets))
	}

	for _, secret := range project.Secrets {
		if secret.Name != secretName+"_"+hash {
			t.Errorf("expected secret name %s to be %s_%s", secret.Name, secretName, hash)
		}

		for _, service := range project.Services {
			for _, cfg := range service.Secrets {
				if cfg.Source == secretName {
					if cfg.Source != secret.Name {
						t.Errorf("expected service secret source to be updated to %s, got %s", secret.Name, cfg.Source)
					}
				}
			}
		}
	}
}
