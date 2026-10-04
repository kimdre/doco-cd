package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/kimdre/doco-cd/internal/source/store"
)

// discoveryTestSource is a Git source repository with a store that mirrors it.
type discoveryTestSource struct {
	root     string
	repo     *git.Repository
	store    *store.GitStore
	artifact store.Artifact
	revision store.Revision
}

// newDiscoveryTestSource commits files to the main branch of a new repository and publishes it.
func newDiscoveryTestSource(t *testing.T, files map[string]string) *discoveryTestSource {
	t.Helper()

	root := t.TempDir()

	repo, err := git.PlainInitWithOptions(root, &git.PlainInitOptions{DefaultBranch: DefaultReference})
	if err != nil {
		t.Fatal(err)
	}

	s := &discoveryTestSource{root: root, repo: repo}
	s.commit(t, "initial", files)

	s.store, err = store.NewGitStore(store.GitStoreOptions{CloneURL: root, BaseDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	s.revision = s.resolve(t, "main")

	s.artifact, err = s.store.Publish(context.Background(), s.revision)
	if err != nil {
		t.Fatal(err)
	}

	return s
}

// commit writes files, where a value prefixed with "symlink:" creates a symlink, and commits them.
func (s *discoveryTestSource) commit(t *testing.T, message string, files map[string]string) plumbing.Hash {
	t.Helper()

	for name, content := range files {
		target := filepath.Join(s.root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			t.Fatal(err)
		}

		if link, ok := strings.CutPrefix(content, "symlink:"); ok {
			if err := os.Symlink(link, target); err != nil {
				t.Fatal(err)
			}

			continue
		}

		if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := commitAll(t, s.repo, message); err != nil {
		t.Fatal(err)
	}

	head, err := s.repo.Head()
	if err != nil {
		t.Fatal(err)
	}

	return head.Hash()
}

// checkout switches the worktree to branch, creating it if create is true.
func (s *discoveryTestSource) checkout(t *testing.T, branch string, create bool) {
	t.Helper()

	worktree, err := s.repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	if err := worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(branch),
		Create: create,
	}); err != nil {
		t.Fatal(err)
	}
}

func (s *discoveryTestSource) resolve(t *testing.T, reference string) store.Revision {
	t.Helper()

	revision, err := s.store.Resolve(context.Background(), reference)
	if err != nil {
		t.Fatal(err)
	}

	return revision
}

func (s *discoveryTestSource) gitOptions() *GitOptions {
	return &GitOptions{SourceURL: s.root}
}

func configNames(configs []*Config) []string {
	names := make([]string, 0, len(configs))
	for _, c := range configs {
		names = append(names, c.Name)
	}

	slices.Sort(names)

	return names
}

func TestGetConfigs_AutoDiscoveryUsesJobRevision(t *testing.T) {
	for name, rootConfig := range map[string]string{
		"without reference": "auto_discovery: true\n",
		"with reference":    "reference: main\nauto_discovery: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			source := newDiscoveryTestSource(t, map[string]string{
				".doco-cd.yaml":      rootConfig,
				"alpha/compose.yaml": "services: {}\n",
				"beta/compose.yaml":  "services: {}\n",
			})

			// The mirror moves past the job's revision while the job is still running.
			source.commit(t, "add gamma", map[string]string{"gamma/compose.yaml": "services: {}\n"})

			if tip := source.resolve(t, "main"); tip == source.revision {
				t.Fatal("test setup: the mirror tip must advance")
			}

			configs, err := GetConfigs(context.Background(), source.artifact.Path, ".", "", DefaultReference,
				source.store.MirrorDir(), string(source.revision), source.gitOptions())
			if err != nil {
				t.Fatal(err)
			}

			if got := configNames(configs); !slices.Equal(got, []string{"alpha", "beta"}) {
				t.Fatalf("discovered %v, want the stacks of the job's revision [alpha beta]", got)
			}

			for _, c := range configs {
				origin := c.Internal.AutoDiscoveryOrigin
				if origin == nil || origin.Revision != string(source.revision) || origin.WorkingDirectory != "." {
					t.Fatalf("config %s has origin %+v, want working dir . and revision %s", c.Name, origin, source.revision)
				}
			}
		})
	}
}

// newAlternateReferenceSource returns a source whose main branch auto-discovers the feature branch.
// The feature branch holds a stack whose nested config is a symlink.
func newAlternateReferenceSource(t *testing.T, mainConfig string) (*discoveryTestSource, plumbing.Hash) {
	t.Helper()

	source := newDiscoveryTestSource(t, map[string]string{".doco-cd.yaml": mainConfig})

	source.checkout(t, "feature", true)
	feature := source.commit(t, "feature stacks", map[string]string{
		"stacks/alpha/compose.yaml":  "services: {}\n",
		"stacks/alpha/.doco-cd.yaml": "symlink:../../shared/alpha.yaml",
		"shared/alpha.yaml":          "name: renamed-alpha\n",
	})
	source.checkout(t, "main", false)

	return source, feature
}

func TestGetConfigs_AutoDiscoveryAlternateReferenceReadsPublishedArtifact(t *testing.T) {
	source, feature := newAlternateReferenceSource(t,
		"reference: feature\nworking_dir: stacks\nauto_discovery: true\n")

	configs, err := GetConfigs(context.Background(), source.artifact.Path, ".", "", DefaultReference,
		source.store.MirrorDir(), string(source.revision), source.gitOptions())
	if err != nil {
		t.Fatal(err)
	}

	if got := configNames(configs); !slices.Equal(got, []string{"renamed-alpha"}) {
		t.Fatalf("discovered %v, want the feature stack named by its symlinked nested config", got)
	}

	if origin := configs[0].Internal.AutoDiscoveryOrigin; origin == nil ||
		origin.Revision != feature.String() || origin.Reference != "feature" || origin.WorkingDirectory != "stacks" {
		t.Fatalf("origin = %+v, want reference feature, working dir stacks and revision %s", origin, feature)
	}

	if _, err := os.Stat(filepath.Join(filepath.Dir(source.artifact.Path), store.ArtifactDirName(store.Revision(feature.String())))); err != nil {
		t.Fatalf("the feature revision must be published: %v", err)
	}
}

func TestResolveConfigs_InlineAutoDiscoveryHonorsReference(t *testing.T) {
	source, feature := newAlternateReferenceSource(t, "auto_discovery: true\n")

	configs, err := ResolveConfigs(context.Background(), []*Config{{
		Reference:        "feature",
		WorkingDirectory: "stacks",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscovery:    AutoDiscoveryConfig{Enabled: true},
	}}, "", DefaultReference, source.artifact.Path, ".", source.store.MirrorDir(), string(source.revision),
		source.gitOptions())
	if err != nil {
		t.Fatal(err)
	}

	if got := configNames(configs); !slices.Equal(got, []string{"renamed-alpha"}) {
		t.Fatalf("discovered %v, want the stack of the feature branch", got)
	}

	if origin := configs[0].Internal.AutoDiscoveryOrigin; origin == nil || origin.Revision != feature.String() {
		t.Fatalf("origin = %+v, want revision %s", origin, feature)
	}
}

func TestGetConfigs_RejectsNegativeAutoDiscoveryDepth(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := createTestFile(t, filepath.Join(dir, ".doco-cd.yaml"),
		"auto_discovery:\n  enabled: true\n  depth: -1\n"); err != nil {
		t.Fatal(err)
	}

	_, err := GetConfigs(context.Background(), dir, ".", "", DefaultReference, "", "", nil)
	if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("GetConfigs() error = %v, want an invalid depth error", err)
	}
}

func TestAutoDiscoveryOrigin_Owns(t *testing.T) {
	t.Parallel()

	tests := []struct {
		workingDir string
		depth      int
		dir        string
		want       bool
	}{
		{".", 0, ".", true},
		{".", 0, "a/b/c", true},
		{".", 1, "a", true},
		{".", 1, "a/b", false},
		{"stacks", 0, "stacks", true},
		{"stacks", 0, "stacks/a/b", true},
		{"stacks", 1, "stacks/a", true},
		{"stacks", 1, "stacks/a/b", false},
		{"stacks", 0, "other/a", false},
		{"stacks", 0, "stacks-old/a", false},
		{"stacks", 0, "stacks/../other", false},
		{"./stacks/", 2, "stacks/a/b", true},
		{".", 0, "../a", false},
	}

	for _, tt := range tests {
		origin := &AutoDiscoveryOrigin{
			WorkingDirectory: tt.workingDir,
			Settings:         AutoDiscoveryConfig{Enabled: true, ScanDepth: tt.depth},
		}

		if got := origin.Owns(tt.dir); got != tt.want {
			t.Errorf("Owns(%q) with working dir %q and depth %d = %v, want %v",
				tt.dir, tt.workingDir, tt.depth, got, tt.want)
		}
	}

	if (*AutoDiscoveryOrigin)(nil).Owns(".") {
		t.Error("a nil origin must not own any directory")
	}
}

func TestGetConfigs_NestedConfigOverridesWithZeroValues(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	files := map[string]string{
		".doco-cd.yaml": `auto_discovery: true
remove_orphans: true
destroy:
  enabled: true
  remove_volumes: true
  remove_images: true
`,
		"alpha/compose.yaml": "services: {}\n",
		"alpha/.doco-cd.yaml": `remove_orphans: false
destroy:
  remove_volumes: false
`,
		"beta/compose.yaml":   "services: {}\n",
		"beta/.doco-cd.yaml":  "destroy: false\n",
		"gamma/compose.yaml":  "services: {}\n",
		"gamma/.doco-cd.yaml": "destroy:\n  remove_volumes: null\n",
	}

	for name, content := range files {
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := createTestFile(t, target, content); err != nil {
			t.Fatal(err)
		}
	}

	configs, err := GetConfigs(context.Background(), dir, ".", "", DefaultReference, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	byName := make(map[string]*Config, len(configs))
	for _, c := range configs {
		byName[c.Name] = c
	}

	alpha := byName["alpha"]
	if alpha == nil || alpha.RemoveOrphans || !alpha.Destroy.Enabled || alpha.Destroy.RemoveVolumes || !alpha.Destroy.RemoveImages {
		t.Fatalf("alpha = %+v, want remove_orphans and destroy.remove_volumes overridden with false", alpha)
	}

	beta := byName["beta"]
	if beta == nil || beta.Destroy.Enabled || !beta.Destroy.RemoveVolumes || !beta.RemoveOrphans {
		t.Fatalf("beta = %+v, want only destroy.enabled overridden by the shorthand", beta)
	}

	gamma := byName["gamma"]
	if gamma == nil || !gamma.Destroy.RemoveVolumes {
		t.Fatalf("gamma = %+v, want a null value to keep the inherited setting", gamma)
	}
}

func TestGetConfigs_NestedYAMLMergePrecedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		nested string
		want   DestroyConfig
	}{
		{
			name:   "explicit false replaces merged mapping",
			nested: "base: &base\n  destroy: {remove_volumes: true}\n<<: *base\ndestroy: false\n",
			want:   DestroyConfig{RemoveVolumes: true, RemoveImages: true},
		},
		{
			name:   "explicit false wins before merge key",
			nested: "base: &base\n  destroy: {remove_volumes: true}\ndestroy: false\n<<: *base\n",
			want:   DestroyConfig{RemoveVolumes: true, RemoveImages: true},
		},
		{
			name:   "explicit null discards merged mapping and inherits",
			nested: "base: &base\n  destroy: {enabled: false, remove_volumes: false}\n<<: *base\ndestroy: null\n",
			want:   DestroyConfig{Enabled: true, RemoveVolumes: true, RemoveImages: true},
		},
		{
			name:   "explicit mapping replaces merged children",
			nested: "base: &base\n  destroy: {enabled: false, remove_volumes: false}\n<<: *base\ndestroy: {remove_images: false}\n",
			want:   DestroyConfig{Enabled: true, RemoveVolumes: true},
		},
		{
			name:   "merge sequence first scalar wins",
			nested: "first: &first\n  destroy: false\nsecond: &second\n  destroy: {remove_volumes: false}\n<<: [*first, *second]\n",
			want:   DestroyConfig{RemoveVolumes: true, RemoveImages: true},
		},
		{
			name:   "merge sequence first mapping replaces later children",
			nested: "first: &first\n  destroy: {remove_images: false}\nsecond: &second\n  destroy: {enabled: false, remove_volumes: false}\n<<: [*first, *second]\n",
			want:   DestroyConfig{Enabled: true, RemoveVolumes: true},
		},
		{
			name:   "merge sequence first null shadows later mapping",
			nested: "first: &first\n  destroy: null\nsecond: &second\n  destroy: {enabled: false, remove_volumes: false}\n<<: [*first, *second]\n",
			want:   DestroyConfig{Enabled: true, RemoveVolumes: true, RemoveImages: true},
		},
		{
			name:   "explicit null child shadows merged child and inherits",
			nested: "base: &base\n  enabled: false\n  remove_volumes: false\ndestroy:\n  <<: *base\n  remove_volumes: null\n",
			want:   DestroyConfig{RemoveVolumes: true, RemoveImages: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			for name, contents := range map[string]string{
				".doco-cd.yaml":       "auto_discovery: true\ndestroy: {enabled: true, remove_volumes: true, remove_images: true}\n",
				"alpha/compose.yaml":  "services: {}\n",
				"alpha/.doco-cd.yaml": tt.nested,
			} {
				target := filepath.Join(dir, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
					t.Fatal(err)
				}

				if err := createTestFile(t, target, contents); err != nil {
					t.Fatal(err)
				}
			}

			configs, err := GetConfigs(t.Context(), dir, ".", "", DefaultReference, "", "", nil)
			if err != nil {
				t.Fatal(err)
			}

			if len(configs) != 1 {
				t.Fatalf("discovered %d configs, want one", len(configs))
			}

			if configs[0].Destroy != tt.want {
				t.Fatalf("nested destroy = %+v, want %+v", configs[0].Destroy, tt.want)
			}
		})
	}
}

func TestYAMLKeysOf(t *testing.T) {
	t.Parallel()

	keys, err := yamlKeysOf([]byte(`base: &base
  remove_volumes: false
destroy:
  <<: *base
  remove_images: false
build: *base
timeout: null
name: app
`))
	if err != nil {
		t.Fatal(err)
	}

	destroy, ok := keys.children["destroy"]
	if !ok {
		t.Fatal("destroy must be set")
	}

	for _, key := range []string{"remove_volumes", "remove_images"} {
		if _, ok := destroy.children[key]; !ok {
			t.Errorf("destroy.%s must be set through the merge key", key)
		}
	}

	if _, ok := destroy.children["<<"]; ok {
		t.Error("a merge key must not be recorded as a key")
	}

	if _, ok := keys.children["build"].children["remove_volumes"]; !ok {
		t.Error("an alias must contribute the keys of its anchor")
	}

	if _, ok := keys.children["timeout"]; ok {
		t.Error("a null value must not count as set")
	}

	if name := keys.children["name"]; name == nil || name.children != nil {
		t.Errorf("name = %+v, want a scalar key", name)
	}

	if got := keys.children["name"].forStruct(); got.children == nil || got.children["enabled"] == nil {
		t.Errorf("a scalar struct value must set its enabled key, got %+v", got)
	}
}

func TestYAMLKeysOf_RejectsAliasExpansion(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	b.WriteString("l0: &l0 {a: 1, b: 1, c: 1, d: 1, e: 1, f: 1, g: 1, h: 1, i: 1, j: 1}\n")

	for i := 1; i <= 8; i++ {
		prev := "*l" + string(rune('0'+i-1))
		b.WriteString("l" + string(rune('0'+i)) + ": &l" + string(rune('0'+i)) + " {")

		for j := range 10 {
			if j > 0 {
				b.WriteString(", ")
			}

			b.WriteString(string(rune('a'+j)) + ": " + prev)
		}

		b.WriteString("}\n")
	}

	if _, err := yamlKeysOf([]byte(b.String())); !errors.Is(err, errTooManyYAMLKeys) {
		t.Fatalf("yamlKeysOf() error = %v, want %v", err, errTooManyYAMLKeys)
	}
}
