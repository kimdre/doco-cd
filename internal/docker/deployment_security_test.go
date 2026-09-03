package docker

import (
	"path/filepath"
	"testing"
)

func TestResolveExternalWorkingDirContainment(t *testing.T) {
	t.Parallel()

	repoPath := filepath.Join(t.TempDir(), "repository")

	tests := []struct {
		name       string
		workingDir string
		wantPath   string
		wantErr    bool
	}{
		{name: "repository root", workingDir: ".", wantPath: repoPath},
		{name: "nested directory", workingDir: "deploy/production", wantPath: filepath.Join(repoPath, "deploy/production")},
		{name: "normalized nested directory", workingDir: "deploy/../production", wantPath: filepath.Join(repoPath, "production")},
		{name: "parent traversal", workingDir: "..", wantPath: filepath.Dir(repoPath), wantErr: true},
		{name: "sibling prefix", workingDir: "../repository-backup", wantPath: repoPath + "-backup", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveExternalWorkingDir(repoPath, tt.workingDir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveExternalWorkingDir() error = %v, wantErr %v", err, tt.wantErr)
			}

			if got != tt.wantPath {
				t.Fatalf("resolveExternalWorkingDir() = %q, want %q", got, tt.wantPath)
			}
		})
	}
}
