package docker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/kimdre/doco-cd/internal/filesystem"
)

func TestProjectPathFilter(t *testing.T) {
	writeFile := func(t *testing.T, path string) string {
		t.Helper()

		if err := os.MkdirAll(filepath.Dir(path), filesystem.PermDir); err != nil {
			t.Fatalf("failed to create %s: %v", filepath.Dir(path), err)
		}

		if err := os.WriteFile(path, []byte("test\n"), filesystem.PermOwner); err != nil {
			t.Fatalf("failed to write %s: %v", path, err)
		}

		return path
	}

	mkDir := func(t *testing.T, path string) string {
		t.Helper()

		if err := os.MkdirAll(path, filesystem.PermDir); err != nil {
			t.Fatalf("failed to create %s: %v", path, err)
		}

		return path
	}

	testCases := []struct {
		name string
		// buildProject gets the repository root and returns the project to filter for.
		buildProject func(t *testing.T, repoPath string) *types.Project
		// wantNilFilter expects no filter at all, meaning every commit is relevant.
		wantNilFilter bool
		matching      []string
		notMatching   []string
	}{
		{
			name: "compose file, env file and config of the stack",
			buildProject: func(t *testing.T, repoPath string) *types.Project {
				t.Helper()

				workingDir := mkDir(t, filepath.Join(repoPath, "stacks", "a"))

				return &types.Project{
					WorkingDir:   workingDir,
					ComposeFiles: []string{writeFile(t, filepath.Join(workingDir, "compose.yaml"))},
					Services: types.Services{
						"svc": {
							Name:     "svc",
							EnvFiles: []types.EnvFile{{Path: writeFile(t, filepath.Join(workingDir, ".env"))}},
							Configs:  []types.ServiceConfigObjConfig{{Source: "app_config"}},
						},
					},
					Configs: types.Configs{
						"app_config": {File: writeFile(t, filepath.Join(workingDir, "config", "app.yaml"))},
					},
				}
			},
			matching: []string{
				"stacks/a/compose.yaml",
				"stacks/a/.env",
				"stacks/a/config/app.yaml",
			},
			notMatching: []string{
				"stacks/b/compose.yaml",
				"stacks/a/compose.yaml.bak",
				"stacks/a/config/other.yaml",
				"README.md",
			},
		},
		{
			name: "bind mounted directory matches everything below it",
			buildProject: func(t *testing.T, repoPath string) *types.Project {
				t.Helper()

				workingDir := mkDir(t, filepath.Join(repoPath, "stacks", "a"))
				dataDir := mkDir(t, filepath.Join(workingDir, "data"))
				bindFile := writeFile(t, filepath.Join(workingDir, "nginx.conf"))

				return &types.Project{
					WorkingDir:   workingDir,
					ComposeFiles: []string{writeFile(t, filepath.Join(workingDir, "compose.yaml"))},
					Services: types.Services{
						"svc": {
							Name: "svc",
							Volumes: []types.ServiceVolumeConfig{
								{Type: "bind", Source: dataDir, Target: "/data"},
								{Type: "bind", Source: bindFile, Target: "/etc/nginx/nginx.conf"},
								{Type: "volume", Source: "named", Target: "/var/lib/data"},
							},
						},
					},
				}
			},
			matching: []string{
				"stacks/a/data/nested/file.txt",
				"stacks/a/data/file.txt",
				"stacks/a/nginx.conf",
			},
			notMatching: []string{
				"stacks/a/database/file.txt",
				"stacks/a/nginx.conf.tpl",
				"named",
			},
		},
		{
			name: "build context and its files",
			buildProject: func(t *testing.T, repoPath string) *types.Project {
				t.Helper()

				workingDir := mkDir(t, filepath.Join(repoPath, "stacks", "a"))
				buildContext := mkDir(t, filepath.Join(repoPath, "src"))

				writeFile(t, filepath.Join(buildContext, "Dockerfile"))

				return &types.Project{
					WorkingDir:   workingDir,
					ComposeFiles: []string{writeFile(t, filepath.Join(workingDir, "compose.yaml"))},
					Services: types.Services{
						"svc": {
							Name: "svc",
							Build: &types.BuildConfig{
								Context:    buildContext,
								Dockerfile: "Dockerfile",
							},
						},
					},
				}
			},
			matching: []string{
				"src/main.go",
				"src/Dockerfile",
				"stacks/a/compose.yaml",
			},
			notMatching: []string{
				"stacks/b/compose.yaml",
				"srcs/main.go",
			},
		},
		{
			name: "project without any file resolves to no filter",
			buildProject: func(t *testing.T, repoPath string) *types.Project {
				t.Helper()

				return &types.Project{
					WorkingDir: repoPath,
					Services: types.Services{
						"svc": {Name: "svc", Image: "nginx:alpine"},
					},
				}
			},
			wantNilFilter: true,
		},
		{
			name: "files outside the repository are ignored",
			buildProject: func(t *testing.T, repoPath string) *types.Project {
				t.Helper()

				outside := mkDir(t, filepath.Join(filepath.Dir(repoPath), "outside"))
				workingDir := mkDir(t, filepath.Join(repoPath, "stacks", "a"))

				return &types.Project{
					WorkingDir:   workingDir,
					ComposeFiles: []string{writeFile(t, filepath.Join(workingDir, "compose.yaml"))},
					Services: types.Services{
						"svc": {
							Name: "svc",
							Volumes: []types.ServiceVolumeConfig{
								{Type: "bind", Source: outside, Target: "/data"},
							},
						},
					},
				}
			},
			matching:    []string{"stacks/a/compose.yaml"},
			notMatching: []string{"outside/file.txt", "stacks/b/compose.yaml"},
		},
		{
			name: "stack covering the repository root resolves to no filter",
			buildProject: func(t *testing.T, repoPath string) *types.Project {
				t.Helper()

				return &types.Project{
					WorkingDir:   repoPath,
					ComposeFiles: []string{writeFile(t, filepath.Join(repoPath, "compose.yaml"))},
					Services: types.Services{
						"svc": {
							Name: "svc",
							Volumes: []types.ServiceVolumeConfig{
								{Type: "bind", Source: repoPath, Target: "/repo"},
							},
						},
					},
				}
			},
			wantNilFilter: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// The repository lives in a subdirectory so a test can also place files next to it.
			repoPath := mkDir(t, filepath.Join(t.TempDir(), "repo"))

			filter, err := ProjectPathFilter(repoPath, tc.buildProject(t, repoPath))
			if err != nil {
				t.Fatalf("ProjectPathFilter returned unexpected error: %v", err)
			}

			if tc.wantNilFilter {
				if filter != nil {
					t.Fatal("expected no filter")
				}

				return
			}

			if filter == nil {
				t.Fatal("expected a filter, got nil")
			}

			for _, p := range tc.matching {
				if !filter(p) {
					t.Errorf("expected %q to match", p)
				}
			}

			for _, p := range tc.notMatching {
				if filter(p) {
					t.Errorf("expected %q not to match", p)
				}
			}
		})
	}
}

func TestProjectPathFilter_NilProject(t *testing.T) {
	filter, err := ProjectPathFilter(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("ProjectPathFilter returned unexpected error: %v", err)
	}

	if filter != nil {
		t.Fatal("expected no filter for a nil project")
	}
}
