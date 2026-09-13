package docker

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/kimdre/doco-cd/internal/git"
)

// layoutStep is one commit of a layout: the files it writes and the files it deletes.
type layoutStep struct {
	msg    string
	write  map[string]string
	remove []string
	// git runs raw git commands instead of a write/commit, for histories a plain commit
	// cannot express, e.g. a merge.
	git [][]string
	// fn builds the step itself, for layouts that need a second repository.
	fn func(t *testing.T, repoDir string, n int)
}

// changelogLayout is one repository layout the changelog filter is checked against.
type changelogLayout struct {
	name string
	// files is the tree of the baseline commit, the commit the stack was deployed from.
	files map[string]string
	// steps are the commits made after the baseline.
	steps []layoutStep
	// workingDir, composeFiles and envFiles address the stack, relative to the repo root.
	workingDir   string
	composeFiles []string
	envFiles     []string
	profiles     []string
	// want are the commit subjects the changelog of the stack should have, newest first.
	want []string
	// note documents why a layout behaves the way it does.
	note string
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()

	return runGitAt(t, dir, 0, args...)
}

// runGitAt runs a git command with the author and committer date derived from n, so the
// committer time order of a built history is deterministic.
func runGitAt(t *testing.T, dir string, n int, args ...string) string {
	t.Helper()

	date := fmt.Sprintf("2026-01-01T%02d:00:00+00:00", n)

	cmd := exec.Command("git", args...)
	cmd.Dir = dir

	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return string(out)
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()

	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// buildLayoutRepo creates a repository for the layout and returns its path, the baseline
// commit and HEAD.
func buildLayoutRepo(t *testing.T, l changelogLayout) (string, plumbing.Hash, plumbing.Hash) {
	t.Helper()

	dir := t.TempDir()
	// macOS hands out /var/... symlinks, compose resolves paths to /private/var/...
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	runGit(t, dir, "init", "-b", "main", "-q")
	writeTree(t, dir, l.files)
	runGit(t, dir, "add", "-A")
	commitAll(t, dir, "baseline", 0)

	baseline := plumbing.NewHash(strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD")))

	for i, step := range l.steps {
		if step.fn != nil {
			step.fn(t, dir, i+1)

			continue
		}

		if len(step.git) > 0 {
			for _, args := range step.git {
				runGitAt(t, dir, i+1, args...)
			}

			continue
		}

		writeTree(t, dir, step.write)

		for _, rel := range step.remove {
			if err := os.RemoveAll(filepath.Join(dir, rel)); err != nil {
				t.Fatal(err)
			}
		}

		runGit(t, dir, "add", "-A")
		commitAll(t, dir, step.msg, i+1)
	}

	head := plumbing.NewHash(strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD")))

	return dir, baseline, head
}

// commitAll commits the index with the date derived from n.
func commitAll(t *testing.T, dir, msg string, n int) {
	t.Helper()

	runGitAt(t, dir, n, "commit", "-q", "--allow-empty", "-m", msg)
}

func loadLayoutProject(t *testing.T, repoDir string, l changelogLayout) *types.Project {
	t.Helper()

	workingDir := filepath.Join(repoDir, l.workingDir)

	composeFiles := make([]string, 0, len(l.composeFiles))
	for _, f := range l.composeFiles {
		composeFiles = append(composeFiles, filepath.Join(repoDir, f))
	}

	envFiles := make([]string, 0, len(l.envFiles))
	for _, f := range l.envFiles {
		envFiles = append(envFiles, filepath.Join(repoDir, f))
	}

	project, err := LoadCompose(t.Context(), nil, repoDir, workingDir, "layout-test",
		composeFiles, envFiles, l.profiles, map[string]string{}, ComposeLoadOptions{})
	if err != nil {
		t.Fatalf("LoadCompose: %v", err)
	}

	return project
}

const (
	stackACompose = `services:
  app:
    image: nginx:alpine
`
	stackBCompose = `services:
  app:
    image: redis:alpine
`
)

// changelogLayouts is the matrix of repository layouts the changelog filter is proven
// against. Every case builds a real repository, loads the stack with the real compose
// loader and compares the changelog both to the expectation and to `git log -- <paths>`.
func changelogLayouts() []changelogLayout {
	return []changelogLayout{
		{
			name: "stack per directory",
			files: map[string]string{
				"stacks/a/compose.yaml": stackACompose,
				"stacks/b/compose.yaml": stackBCompose,
			},
			steps: []layoutStep{
				{msg: "touch b", write: map[string]string{"stacks/b/compose.yaml": stackBCompose + "# b\n"}},
				{msg: "touch a", write: map[string]string{"stacks/a/compose.yaml": stackACompose + "# a\n"}},
				{msg: "touch b again", write: map[string]string{"stacks/b/compose.yaml": stackBCompose + "# bb\n"}},
			},
			workingDir:   "stacks/a",
			composeFiles: []string{"stacks/a/compose.yaml"},
			want:         []string{"touch a"},
		},
		{
			name: "shared env file at repo root",
			files: map[string]string{
				"shared.env": "TAG=1\n",
				"stacks/a/compose.yaml": `services:
  app:
    image: nginx:alpine
    env_file:
      - ../../shared.env
`,
				"stacks/b/compose.yaml": stackBCompose,
			},
			steps: []layoutStep{
				{msg: "touch b", write: map[string]string{"stacks/b/compose.yaml": stackBCompose + "# b\n"}},
				{msg: "bump shared env", write: map[string]string{"shared.env": "TAG=2\n"}},
			},
			workingDir:   "stacks/a",
			composeFiles: []string{"stacks/a/compose.yaml"},
			want:         []string{"bump shared env"},
		},
		{
			name: "bind mounted directory",
			files: map[string]string{
				"stacks/a/config/app.conf": "a\n",
				"stacks/a/compose.yaml": `services:
  app:
    image: nginx:alpine
    volumes:
      - ./config:/etc/app
`,
				"stacks/b/compose.yaml": stackBCompose,
			},
			steps: []layoutStep{
				{msg: "touch b", write: map[string]string{"stacks/b/compose.yaml": stackBCompose + "# b\n"}},
				{msg: "add nested config", write: map[string]string{"stacks/a/config/nested/extra.conf": "x\n"}},
				{msg: "remove config file", remove: []string{"stacks/a/config/app.conf"}},
			},
			workingDir:   "stacks/a",
			composeFiles: []string{"stacks/a/compose.yaml"},
			want:         []string{"remove config file", "add nested config"},
		},
		{
			name: "build context outside the stack directory",
			files: map[string]string{
				"app/main.go":           "package main\n",
				"app/Dockerfile":        "FROM scratch\n",
				"stacks/b/compose.yaml": stackBCompose,
				"stacks/a/compose.yaml": `services:
  app:
    build:
      context: ../../app
`,
			},
			steps: []layoutStep{
				{msg: "touch b", write: map[string]string{"stacks/b/compose.yaml": stackBCompose + "# b\n"}},
				{msg: "change app source", write: map[string]string{"app/main.go": "package main // v2\n"}},
			},
			workingDir:   "stacks/a",
			composeFiles: []string{"stacks/a/compose.yaml"},
			want:         []string{"change app source"},
		},
		{
			name: "dockerfile outside the build context",
			files: map[string]string{
				"app/main.go":           "package main\n",
				"docker/app.Dockerfile": "FROM scratch\n",
				"stacks/b/compose.yaml": stackBCompose,
				"stacks/a/compose.yaml": `services:
  app:
    build:
      context: ../../app
      dockerfile: ../docker/app.Dockerfile
`,
			},
			steps: []layoutStep{
				{msg: "touch b", write: map[string]string{"stacks/b/compose.yaml": stackBCompose + "# b\n"}},
				{msg: "change dockerfile", write: map[string]string{"docker/app.Dockerfile": "FROM alpine\n"}},
			},
			workingDir:   "stacks/a",
			composeFiles: []string{"stacks/a/compose.yaml"},
			want:         []string{"change dockerfile"},
		},
		{
			name: "compose override file",
			files: map[string]string{
				"stacks/a/compose.yaml":          stackACompose,
				"stacks/a/compose.override.yaml": "services:\n  app:\n    ports:\n      - 8080:80\n",
				"stacks/b/compose.yaml":          stackBCompose,
			},
			steps: []layoutStep{
				{msg: "touch b", write: map[string]string{"stacks/b/compose.yaml": stackBCompose + "# b\n"}},
				{msg: "change override", write: map[string]string{"stacks/a/compose.override.yaml": "services:\n  app:\n    ports:\n      - 8081:80\n"}},
			},
			workingDir:   "stacks/a",
			composeFiles: []string{"stacks/a/compose.yaml", "stacks/a/compose.override.yaml"},
			want:         []string{"change override"},
		},
		{
			name: "top level config and secret files",
			files: map[string]string{
				"stacks/a/nginx.conf":   "server {}\n",
				"stacks/a/token.txt":    "t\n",
				"stacks/b/compose.yaml": stackBCompose,
				"stacks/a/compose.yaml": `services:
  app:
    image: nginx:alpine
    configs:
      - nginx
    secrets:
      - token
configs:
  nginx:
    file: ./nginx.conf
secrets:
  token:
    file: ./token.txt
`,
			},
			steps: []layoutStep{
				{msg: "touch b", write: map[string]string{"stacks/b/compose.yaml": stackBCompose + "# b\n"}},
				{msg: "change nginx config", write: map[string]string{"stacks/a/nginx.conf": "server { listen 80; }\n"}},
				{msg: "rotate token", write: map[string]string{"stacks/a/token.txt": "t2\n"}},
			},
			workingDir:   "stacks/a",
			composeFiles: []string{"stacks/a/compose.yaml"},
			want:         []string{"rotate token", "change nginx config"},
		},
		{
			name: "bind mount outside the repository",
			files: map[string]string{
				"stacks/b/compose.yaml": stackBCompose,
				"stacks/a/compose.yaml": `services:
  app:
    image: nginx:alpine
    volumes:
      - /etc:/host-etc:ro
`,
			},
			steps: []layoutStep{
				{msg: "touch b", write: map[string]string{"stacks/b/compose.yaml": stackBCompose + "# b\n"}},
				{msg: "touch a", write: map[string]string{"stacks/a/compose.yaml": stackACompose}},
			},
			workingDir:   "stacks/a",
			composeFiles: []string{"stacks/a/compose.yaml"},
			want:         []string{"touch a"},
			note:         "host paths are dropped, they cannot appear in the log",
		},
		{
			name: "stack at the repository root",
			files: map[string]string{
				"compose.yaml": stackACompose,
				"other.txt":    "x\n",
			},
			steps: []layoutStep{
				{msg: "touch unrelated file", write: map[string]string{"other.txt": "y\n"}},
				{msg: "touch compose", write: map[string]string{"compose.yaml": stackACompose + "# a\n"}},
			},
			workingDir:   ".",
			composeFiles: []string{"compose.yaml"},
			want:         []string{"touch compose"},
			note:         "a stack at the root still resolves to its own files, only a build context on the root drops the filter",
		},
		{
			name: "deploy config of the stack",
			files: map[string]string{
				".doco-cd.yml":          "name: a\nworking_dir: stacks/a\n",
				"stacks/a/compose.yaml": stackACompose,
				"stacks/b/compose.yaml": stackBCompose,
			},
			steps: []layoutStep{
				{msg: "touch b", write: map[string]string{"stacks/b/compose.yaml": stackBCompose + "# b\n"}},
				{msg: "change deploy config", write: map[string]string{".doco-cd.yml": "name: a\nworking_dir: stacks/a\nforce_recreate: true\n"}},
			},
			workingDir:   "stacks/a",
			composeFiles: []string{"stacks/a/compose.yaml"},
			want:         []string{},
			note:         "GAP: the deploy config is not part of the compose project, so its own change is not reported",
		},
		{
			name: "include of another compose file",
			files: map[string]string{
				"common/base.yaml":      "services:\n  app:\n    image: nginx:alpine\n",
				"stacks/b/compose.yaml": stackBCompose,
				"stacks/a/compose.yaml": `include:
  - ../../common/base.yaml
`,
			},
			steps: []layoutStep{
				{msg: "touch b", write: map[string]string{"stacks/b/compose.yaml": stackBCompose + "# b\n"}},
				{msg: "change included file", write: map[string]string{"common/base.yaml": "services:\n  app:\n    image: nginx:1.27-alpine\n"}},
			},
			workingDir:   "stacks/a",
			composeFiles: []string{"stacks/a/compose.yaml"},
			want:         []string{},
			note:         "GAP: compose-go does not keep the included files in types.Project, so the include is invisible to the filter",
		},
		{
			name: "extends from another file",
			files: map[string]string{
				"common/svc.yaml":       "services:\n  base:\n    image: nginx:alpine\n",
				"stacks/b/compose.yaml": stackBCompose,
				"stacks/a/compose.yaml": `services:
  app:
    extends:
      file: ../../common/svc.yaml
      service: base
`,
			},
			steps: []layoutStep{
				{msg: "touch b", write: map[string]string{"stacks/b/compose.yaml": stackBCompose + "# b\n"}},
				{msg: "change extended file", write: map[string]string{"common/svc.yaml": "services:\n  base:\n    image: nginx:1.27-alpine\n"}},
			},
			workingDir:   "stacks/a",
			composeFiles: []string{"stacks/a/compose.yaml"},
			want:         []string{},
			note:         "GAP: compose-go resolves extends away, the extended file is not kept in types.Project",
		},
		{
			name: "service behind an inactive profile",
			files: map[string]string{
				"stacks/a/debug/tool.conf": "d\n",
				"stacks/b/compose.yaml":    stackBCompose,
				"stacks/a/compose.yaml": `services:
  app:
    image: nginx:alpine
  debug:
    image: busybox
    profiles: [debug]
    volumes:
      - ./debug:/debug
`,
			},
			steps: []layoutStep{
				{msg: "touch b", write: map[string]string{"stacks/b/compose.yaml": stackBCompose + "# b\n"}},
				{msg: "change debug tool config", write: map[string]string{"stacks/a/debug/tool.conf": "d2\n"}},
			},
			workingDir:   "stacks/a",
			composeFiles: []string{"stacks/a/compose.yaml"},
			want:         []string{},
			note:         "the profile is not active, so the files of the disabled service are not part of the stack",
		},
		{
			name: "renamed file inside a bind mounted directory",
			files: map[string]string{
				"stacks/a/config/app.conf": "a\n",
				"stacks/b/compose.yaml":    stackBCompose,
				"stacks/a/compose.yaml": `services:
  app:
    image: nginx:alpine
    volumes:
      - ./config:/etc/app
`,
			},
			steps: []layoutStep{
				{msg: "touch b", write: map[string]string{"stacks/b/compose.yaml": stackBCompose + "# b\n"}},
				{msg: "rename config file", write: map[string]string{"stacks/a/config/renamed.conf": "a\n"}, remove: []string{"stacks/a/config/app.conf"}},
			},
			workingDir:   "stacks/a",
			composeFiles: []string{"stacks/a/compose.yaml"},
			want:         []string{"rename config file"},
		},
		{
			name: "merge commit of a stack branch",
			files: map[string]string{
				"stacks/a/compose.yaml": stackACompose,
				"stacks/b/compose.yaml": stackBCompose,
			},
			steps: []layoutStep{
				{msg: "touch b on main", write: map[string]string{"stacks/b/compose.yaml": stackBCompose + "# b\n"}},
				{git: [][]string{{"checkout", "-q", "-b", "feature", "HEAD~1"}}},
				{msg: "touch a on branch", write: map[string]string{"stacks/a/compose.yaml": stackACompose + "# a\n"}},
				{git: [][]string{
					{"checkout", "-q", "main"},
					{"merge", "-q", "--no-ff", "-m", "merge feature", "feature"},
				}},
			},
			workingDir:   "stacks/a",
			composeFiles: []string{"stacks/a/compose.yaml"},
			want:         []string{"touch a on branch"},
			note:         "git simplifies the merge away because it matches one parent, go-git may not",
		},
		{
			name: "build context on the repository root",
			files: map[string]string{
				"Dockerfile":   "FROM scratch\n",
				"other.txt":    "x\n",
				"compose.yaml": "services:\n  app:\n    build:\n      context: .\n",
			},
			steps: []layoutStep{
				{msg: "touch unrelated file", write: map[string]string{"other.txt": "y\n"}},
				{msg: "touch compose", write: map[string]string{"compose.yaml": "services:\n  app:\n    build:\n      context: .\n# a\n"}},
			},
			workingDir:   ".",
			composeFiles: []string{"compose.yaml"},
			want:         []string{"touch compose", "touch unrelated file"},
			note:         "the context covers the repo root, the filter is dropped and the changelog stays unfiltered",
		},
		{
			name: "stack inside the build context of another stack",
			files: map[string]string{
				"app/main.go":               "package main\n",
				"app/Dockerfile":            "FROM scratch\n",
				"app/stacks/b/compose.yaml": stackBCompose,
				"stacks/a/compose.yaml": `services:
  app:
    build:
      context: ../../app
`,
			},
			steps: []layoutStep{
				{msg: "touch b", write: map[string]string{"app/stacks/b/compose.yaml": stackBCompose + "# b\n"}},
				{msg: "change app source", write: map[string]string{"app/main.go": "package main // v2\n"}},
			},
			workingDir:   "stacks/a",
			composeFiles: []string{"stacks/a/compose.yaml"},
			want:         []string{"change app source", "touch b"},
			note:         "GAP: a build context matches by prefix, so a stack placed inside it leaks into the changelog",
		},
		{
			name: "stack inside a submodule",
			files: map[string]string{
				"stacks/b/compose.yaml": stackBCompose,
			},
			steps: []layoutStep{
				{msg: "touch b", write: map[string]string{"stacks/b/compose.yaml": stackBCompose + "# b\n"}},
				{fn: addSubmoduleStack},
				{fn: bumpSubmoduleStack},
			},
			workingDir:   "sub/stacks/a",
			composeFiles: []string{"sub/stacks/a/compose.yaml"},
			want:         []string{},
			note:         "GAP: a submodule bump changes the gitlink path 'sub', never a path inside the stack",
		},
		{
			name: "env var in the bind mount source",
			files: map[string]string{
				"stacks/a/.env":            "CONF_DIR=config\n",
				"stacks/a/config/app.conf": "a\n",
				"stacks/b/compose.yaml":    stackBCompose,
				"stacks/a/compose.yaml": `services:
  app:
    image: nginx:alpine
    volumes:
      - ./${CONF_DIR}:/etc/app
`,
			},
			steps: []layoutStep{
				{msg: "touch b", write: map[string]string{"stacks/b/compose.yaml": stackBCompose + "# b\n"}},
				{msg: "change interpolated config", write: map[string]string{"stacks/a/config/app.conf": "a2\n"}},
			},
			workingDir:   "stacks/a",
			composeFiles: []string{"stacks/a/compose.yaml"},
			envFiles:     []string{"stacks/a/.env"},
			want:         []string{"change interpolated config"},
		},
	}
}

func TestChangelogFilterLayouts(t *testing.T) {
	for _, l := range changelogLayouts() {
		t.Run(l.name, func(t *testing.T) {
			repoDir, baseline, head := buildLayoutRepo(t, l)

			project := loadLayoutProject(t, repoDir, l)

			filePaths, dirPaths, coversRoot, err := projectRepoPaths(repoDir, project)
			if err != nil {
				t.Fatalf("projectRepoPaths: %v", err)
			}

			pathFilter, err := ProjectPathFilter(repoDir, project)
			if err != nil {
				t.Fatalf("ProjectPathFilter: %v", err)
			}

			repo, err := gogit.PlainOpen(repoDir)
			if err != nil {
				t.Fatalf("PlainOpen: %v", err)
			}

			commits, err := git.GetCommitsBetween(slog.New(slog.DiscardHandler), repo, baseline, head, 50, pathFilter)
			if err != nil {
				t.Fatalf("GetCommitsBetween: %v", err)
			}

			got := make([]string, 0, len(commits))
			for _, c := range commits {
				got = append(got, c.Subject)
			}

			t.Logf("resolved paths: files=%v dirs=%v coversRepoRoot=%v filtered=%v",
				filePaths, dirPaths, coversRoot, pathFilter != nil)

			if l.note != "" {
				t.Log(l.note)
			}

			if !slices.Equal(got, l.want) {
				t.Errorf("changelog mismatch\n got: %v\nwant: %v", got, l.want)
			}

			// Differential check against git itself, over the same path set.
			args := []string{"log", "--format=%s", baseline.String() + ".." + head.String()}
			if pathFilter != nil {
				args = append(args, "--")
				args = append(args, filePaths...)
				args = append(args, dirPaths...)
			}

			oracle := []string{}

			for _, line := range strings.Split(strings.TrimSpace(runGit(t, repoDir, args...)), "\n") {
				if line != "" {
					oracle = append(oracle, line)
				}
			}

			if !slices.Equal(got, oracle) {
				t.Errorf("differs from `git log`\n  ours: %v\ngit log: %v\n(args: %v)", got, oracle, args)
			}
		})
	}
}

// addSubmoduleStack creates a second repository holding the stack and adds it as
// submodule "sub" of the layout repository.
func addSubmoduleStack(t *testing.T, repoDir string, n int) {
	t.Helper()

	subDir := filepath.Join(filepath.Dir(repoDir), "subrepo")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	runGit(t, subDir, "init", "-b", "main", "-q")
	writeTree(t, subDir, map[string]string{"stacks/a/compose.yaml": stackACompose})
	runGit(t, subDir, "add", "-A")
	commitAll(t, subDir, "submodule baseline", n)

	runGitAt(t, repoDir, n, "-c", "protocol.file.allow=always", "submodule", "add", "-q", subDir, "sub")
	runGit(t, repoDir, "add", "-A")
	commitAll(t, repoDir, "add submodule", n)
}

// bumpSubmoduleStack changes the stack inside the submodule and records the new submodule
// commit in the layout repository.
func bumpSubmoduleStack(t *testing.T, repoDir string, n int) {
	t.Helper()

	subDir := filepath.Join(filepath.Dir(repoDir), "subrepo")

	writeTree(t, subDir, map[string]string{"stacks/a/compose.yaml": stackACompose + "# a\n"})
	runGit(t, subDir, "add", "-A")
	commitAll(t, subDir, "change stack in submodule", n)

	runGitAt(t, repoDir, n, "-c", "protocol.file.allow=always", "submodule", "update", "--remote", "--recursive", "sub")
	runGit(t, repoDir, "add", "-A")
	commitAll(t, repoDir, "bump submodule", n)
}
