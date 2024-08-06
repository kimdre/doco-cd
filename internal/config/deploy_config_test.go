package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

var projectName = "test"

func createTestFile(fileName string, content string) error {
	err := os.WriteFile(fileName, []byte(content), 0o644)
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

func TestGetDeployConfig(t *testing.T) {
	fileName := ".compose-deploy.yaml"
	reference := "refs/heads/test"
	workingDirectory := "/test"
	composeFiles := []string{"test.compose.yaml"}

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

	config, err := GetDeployConfig(dirName, projectName)
	if err != nil {
		t.Fatal(err)
	}

	if config.Name != projectName {
		t.Errorf("expected name to be %v, got %s", projectName, config.Name)
	}

	if config.Reference != reference {
		t.Errorf("expected reference to be %v, got %s", reference, config.Reference)
	}

	if config.WorkingDirectory != workingDirectory {
		t.Errorf("expected working directory to be '%v', got '%s'", workingDirectory, config.WorkingDirectory)
	}

	if !reflect.DeepEqual(config.ComposeFiles, composeFiles) {
		t.Errorf("expected compose files to be %v, got %v", composeFiles, config.ComposeFiles)
	}
}

func TestGetDeployConfig_Default(t *testing.T) {
	defaultConfig := DefaultDeployConfig(projectName)

	dirName := createTmpDir(t)
	t.Cleanup(func() {
		err := os.RemoveAll(dirName)
		if err != nil {
			t.Fatal(err)
		}
	})

	config, err := GetDeployConfig(dirName, projectName)
	if err != nil {
		t.Fatal(err)
	}

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
