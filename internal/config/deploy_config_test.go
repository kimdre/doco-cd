package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/validator.v2"

	"github.com/kimdre/doco-cd/internal/filesystem"
)

var projectName = "test"

func createTestFile(fileName string, content string) error {
	err := os.WriteFile(fileName, []byte(content), filesystem.PermOwner)
	if err != nil {
		return err
	}

	return nil
}

func createTmpDir(t *testing.T) string {
	dirName, err := os.MkdirTemp(os.TempDir(), "test-*")
	if err != nil {
		t.Fatal(err)
	}

	return dirName
}

func TestGetDeployConfigs(t *testing.T) {
	t.Run("Valid Config", func(t *testing.T) {
		fileName := ".doco-cd.yaml"
		reference := "refs/heads/test"
		workingDirectory := "/test"
		composeFiles := []string{"test.compose.yaml"}
		customTarget := ""

		deployConfig := fmt.Sprintf(`name: %s
reference: %s
working_dir: %s
compose_files:
  - %s
`, projectName, reference, workingDirectory, composeFiles[0])

		dirName := createTmpDir(t)
		t.Cleanup(func() {
			err := os.RemoveAll(dirName)
			if err != nil {
				t.Fatal(err)
			}
		})

		filePath := filepath.Join(dirName, fileName)

		err := createTestFile(filePath, deployConfig)
		if err != nil {
			t.Fatal(err)
		}

		configs, err := GetDeployConfigs(dirName, projectName, customTarget, reference)
		if err != nil {
			t.Fatal(err)
		}

		if len(configs) != 1 {
			t.Fatalf("expected 1 config, got %d", len(configs))
		}

		config := configs[0]

		if config.Name != projectName {
			t.Errorf("expected name to be %v, got %s", projectName, config.Name)
		}

		if config.Reference != reference {
			t.Errorf("expected reference to be %v, got %s", reference, config.Reference)
		}

		if config.WorkingDirectory != filepath.Join(".", workingDirectory) {
			t.Errorf("expected working directory to be '%v', got '%s'", workingDirectory, config.WorkingDirectory)
		}

		if !reflect.DeepEqual(config.ComposeFiles, composeFiles) {
			t.Errorf("expected compose files to be %v, got %v", composeFiles, config.ComposeFiles)
		}
	})
}

func TestGetDeployConfigs_DefaultValues(t *testing.T) {
	defaultConfig := DefaultDeployConfig(projectName, DefaultReference)

	dirName := createTmpDir(t)
	t.Cleanup(func() {
		err := os.RemoveAll(dirName)
		if err != nil {
			t.Fatal(err)
		}
	})

	configs, err := GetDeployConfigs(dirName, projectName, "", "")
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	config := configs[0]

	if config.Name != projectName {
		t.Errorf("expected name to be %v, got %s", projectName, config.Name)
	}

	if config.Reference != defaultConfig.Reference {
		t.Errorf("expected reference to be %s, got %s", defaultConfig.Reference, config.Reference)
	}

	if config.WorkingDirectory != defaultConfig.WorkingDirectory {
		t.Errorf("expected working directory to be %s, got %s", defaultConfig.WorkingDirectory, config.WorkingDirectory)
	}

	if !reflect.DeepEqual(config.ComposeFiles, defaultConfig.ComposeFiles) {
		t.Errorf("expected compose files to be %v, got %v", defaultConfig.ComposeFiles, config.ComposeFiles)
	}
}

// TestGetDeployConfigs_DuplicateProjectName checks if the function returns an error
// when there are duplicate project names in the config files.
func TestGetDeployConfigs_DuplicateProjectName(t *testing.T) {
	config := DeployConfig{
		Name:             "test",
		Reference:        "refs/heads/test",
		WorkingDirectory: "/test",
		ComposeFiles:     []string{"test.compose.yaml"},
	}

	configs := []*DeployConfig{&config, &config}

	err := validateUniqueProjectNames(configs)
	if !errors.Is(err, ErrDuplicateProjectName) {
		t.Fatal("expected error for duplicate project names, got nil")
	}
}

// TestGetDeployConfigs_InvalidRepositoryURL checks if the function returns an error when the repository URL is an SSH URL
// The init function panics if the validator for HttpUrl is not registered correctly.
func TestGetDeployConfigs_RepositoryURL(t *testing.T) {
	testCases := []struct {
		name        string
		repoUrl     HttpUrl
		expectedErr error
	}{
		{
			name:        "Valid HTTP URL",
			repoUrl:     "https://github.com/kimdre/doco-cd.git",
			expectedErr: nil,
		},
		{
			name:        "Valid HTTPS URL",
			repoUrl:     "https://github.com/kimdre/doco-cd.git",
			expectedErr: nil,
		},
		{
			name:        "Invalid HTTP URL",
			repoUrl:     "github.com/kimdre/doco-cd",
			expectedErr: fmt.Errorf("RepositoryUrl: %w", ErrInvalidHttpUrl),
		},
		{
			name:        "Invalid SSH URL", // SSH Urls are not supported
			repoUrl:     "git@github.com:kimdre/doco-cd.git",
			expectedErr: fmt.Errorf("RepositoryUrl: %w", ErrInvalidHttpUrl),
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			config := DeployConfig{
				Name:          tc.name,
				RepositoryUrl: tc.repoUrl,
			}

			err := validator.Validate(config)
			if err == nil && tc.expectedErr != nil {
				t.Fatalf("expected error %v, got nil", tc.expectedErr)
			}

			if err != nil && strings.Contains(tc.expectedErr.Error(), err.Error()) {
				t.Fatalf("expected error %v, got %v", tc.expectedErr, err)
			}
		})
	}
}
