package docker

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/kimdre/doco-cd/internal/git"
)

// layoutStep is one commit of a layout: the files it writes and the files it deletes.
type layoutStep struct {
	msg    string
	write  map[string]string
	remove []string
	// fn builds the step itself, for histories a single commit cannot express.
	fn func(t *testing.T, r *layoutRepo)
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

// layoutRepo is the repository of a layout, built with go-git.
//
// It lives on the real filesystem instead of memfs, because the compose loader reads the
// compose files, env files and bind mount sources of the stack through the OS filesystem.
type layoutRepo struct {
	t        *testing.T
	repo     *gogit.Repository
	wt       *gogit.Worktree
	dir      string
	baseline plumbing.Hash
	// commits counts the commits made so far and gives every commit its own timestamp, so
	// the committer time order of the history is deterministic.
	commits int
}

func newLayoutRepo(t *testing.T) *layoutRepo {
	t.Helper()

	dir := t.TempDir()
	// macOS hands out /var/... symlinks, compose resolves paths to /private/var/...
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}

	// Point HEAD at "main" before the first commit so it lands there directly.
	headRef := plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))
	if err = repo.Storer.SetReference(headRef); err != nil {
		t.Fatalf("set HEAD to main: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	return &layoutRepo{t: t, repo: repo, wt: wt, dir: dir}
}

// write writes the files and stages them.
func (r *layoutRepo) write(files map[string]string) {
	r.t.Helper()

	writeFiles(r.t, r.dir, files)

	for rel := range files {
		if _, err := r.wt.Add(rel); err != nil {
			r.t.Fatalf("add %s: %v", rel, err)
		}
	}
}

// remove deletes the paths from the worktree and the index.
func (r *layoutRepo) remove(paths []string) {
	r.t.Helper()

	for _, rel := range paths {
		if _, err := r.wt.Remove(rel); err != nil {
			r.t.Fatalf("remove %s: %v", rel, err)
		}
	}
}

// commit commits the index. Without parents the commit goes on top of HEAD.
func (r *layoutRepo) commit(msg string, parents ...plumbing.Hash) plumbing.Hash {
	r.t.Helper()

	r.commits++

	when := time.Date(2026, 1, 1, 0, r.commits, 0, 0, time.UTC)
	sig := &object.Signature{Name: "Test", Email: "test@example.com", When: when}

	hash, err := r.wt.Commit(msg, &gogit.CommitOptions{
		AllowEmptyCommits: true,
		Author:            sig,
		Committer:         sig,
		Parents:           parents,
	})
	if err != nil {
		r.t.Fatalf("commit %q: %v", msg, err)
	}

	return hash
}

func (r *layoutRepo) head() plumbing.Hash {
	r.t.Helper()

	ref, err := r.repo.Head()
	if err != nil {
		r.t.Fatalf("head: %v", err)
	}

	return ref.Hash()
}

// setGitlink points the index entry of a submodule path at a commit of the submodule.
// go-git cannot add a submodule, so the gitlink is written directly.
func (r *layoutRepo) setGitlink(path string, sub plumbing.Hash) {
	r.t.Helper()

	idx, err := r.repo.Storer.Index()
	if err != nil {
		r.t.Fatalf("read index: %v", err)
	}

	entry, err := idx.Entry(path)
	if err != nil {
		entry = idx.Add(path)
	}

	entry.Hash = sub
	entry.Mode = filemode.Submodule

	if err = r.repo.Storer.SetIndex(idx); err != nil {
		r.t.Fatalf("write index: %v", err)
	}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
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

// buildLayoutRepo creates the repository of the layout and returns it together with the
// baseline commit and HEAD.
func buildLayoutRepo(t *testing.T, l changelogLayout) (*layoutRepo, plumbing.Hash, plumbing.Hash) {
	t.Helper()

	r := newLayoutRepo(t)

	r.write(l.files)
	r.baseline = r.commit("baseline")

	for _, step := range l.steps {
		if step.fn != nil {
			step.fn(t, r)

			continue
		}

		r.write(step.write)
		r.remove(step.remove)
		r.commit(step.msg)
	}

	return r, r.baseline, r.head()
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

// mergeStackBranch commits a change of stack a on a side branch and merges it into main,
// so the range contains a merge commit.
func mergeStackBranch(t *testing.T, r *layoutRepo) {
	t.Helper()

	mainHead := r.head()
	branchChange := map[string]string{"stacks/a/compose.yaml": stackACompose + "# a\n"}

	err := r.wt.Checkout(&gogit.CheckoutOptions{
		Hash:   r.baseline,
		Branch: plumbing.NewBranchReferenceName("feature"),
		Create: true,
	})
	if err != nil {
		t.Fatalf("checkout feature: %v", err)
	}

	r.write(branchChange)
	featureHead := r.commit("touch a on branch")

	err = r.wt.Checkout(&gogit.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("main")})
	if err != nil {
		t.Fatalf("checkout main: %v", err)
	}

	r.write(branchChange)
	r.commit("merge feature", mainHead, featureHead)
}

// submoduleDir is where the stack of the submodule layout lives inside the repository.
const submoduleDir = "sub"

// addSubmoduleStack creates a second repository holding the stack and records it as
// submodule of the layout repository.
func addSubmoduleStack(t *testing.T, r *layoutRepo) {
	t.Helper()

	subDir := filepath.Join(r.dir, submoduleDir)

	subRepo, err := gogit.PlainInit(subDir, false)
	if err != nil {
		t.Fatalf("init submodule repo: %v", err)
	}

	subWt, err := subRepo.Worktree()
	if err != nil {
		t.Fatalf("submodule worktree: %v", err)
	}

	writeFiles(t, subDir, map[string]string{"stacks/a/compose.yaml": stackACompose})

	if _, err = subWt.Add("stacks/a/compose.yaml"); err != nil {
		t.Fatalf("add submodule stack: %v", err)
	}

	sig := &object.Signature{Name: "Test", Email: "test@example.com", When: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}

	subHead, err := subWt.Commit("submodule baseline", &gogit.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		t.Fatalf("commit submodule baseline: %v", err)
	}

	r.write(map[string]string{
		".gitmodules": "[submodule \"" + submoduleDir + "\"]\n\tpath = " + submoduleDir + "\n\turl = ./" + submoduleDir + "\n",
	})
	r.setGitlink(submoduleDir, subHead)
	r.commit("add submodule")
}

// bumpSubmoduleStack changes the stack inside the submodule and records the new submodule
// commit in the layout repository.
func bumpSubmoduleStack(t *testing.T, r *layoutRepo) {
	t.Helper()

	subDir := filepath.Join(r.dir, submoduleDir)

	subRepo, err := gogit.PlainOpen(subDir)
	if err != nil {
		t.Fatalf("open submodule repo: %v", err)
	}

	subWt, err := subRepo.Worktree()
	if err != nil {
		t.Fatalf("submodule worktree: %v", err)
	}

	writeFiles(t, subDir, map[string]string{"stacks/a/compose.yaml": stackACompose + "# a\n"})

	if _, err = subWt.Add("stacks/a/compose.yaml"); err != nil {
		t.Fatalf("add submodule change: %v", err)
	}

	sig := &object.Signature{Name: "Test", Email: "test@example.com", When: time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)}

	subHead, err := subWt.Commit("change stack in submodule", &gogit.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		t.Fatalf("commit submodule change: %v", err)
	}

	r.setGitlink(submoduleDir, subHead)
	r.commit("bump submodule")
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
				{fn: mergeStackBranch},
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
			r, baseline, head := buildLayoutRepo(t, l)
			repoDir := r.dir

			project := loadLayoutProject(t, repoDir, l)

			filePaths, dirPaths, coversRoot, err := projectRepoPaths(repoDir, project)
			if err != nil {
				t.Fatalf("projectRepoPaths: %v", err)
			}

			pathFilter, err := ProjectPathFilter(repoDir, project)
			if err != nil {
				t.Fatalf("ProjectPathFilter: %v", err)
			}

			commits, err := git.GetCommitsBetween(slog.New(slog.DiscardHandler), r.repo, baseline, head, 50, pathFilter)
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

			// go-git approximates `git log -- <paths>`, so the result is also compared to
			// what git itself reports over the same path set. Only git can answer that,
			// the check is skipped where the binary is missing.
			oracle, args, ok := gitLogOracle(t, repoDir, baseline, head, filePaths, dirPaths, pathFilter != nil)
			if !ok {
				return
			}

			if !slices.Equal(got, oracle) {
				t.Errorf("differs from `git log`\n  ours: %v\ngit log: %v\n(args: %v)", got, oracle, args)
			}
		})
	}
}

// gitLogOracle returns the commit subjects `git log <old>..<new> -- <paths>` reports, and
// whether the git binary was available at all.
func gitLogOracle(t *testing.T, repoDir string, oldHash, newHash plumbing.Hash, files, dirs []string, filtered bool) ([]string, []string, bool) {
	t.Helper()

	bin, err := exec.LookPath("git")
	if err != nil {
		t.Log("git binary not found, skipping the comparison against git log")

		return nil, nil, false
	}

	args := []string{"log", "--format=%s", oldHash.String() + ".." + newHash.String()}
	if filtered {
		args = append(args, "--")
		args = append(args, files...)
		args = append(args, dirs...)
	}

	cmd := exec.Command(bin, args...)
	cmd.Dir = repoDir

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	var subjects []string

	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			subjects = append(subjects, line)
		}
	}

	return subjects, args, true
}
