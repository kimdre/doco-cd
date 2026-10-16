package deploy

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/source/store"
)

func TestGetConfigsWithOriginsRetainsEmptyRoots(t *testing.T) {
	t.Parallel()

	source := newDiscoveryTestSource(t, map[string]string{
		".doco-cd.prod.yaml":                 "working_dir: services\ncontext: production\nauto_discovery: true\n",
		"services/readme.txt":                "preserve the scan root\n",
		"databases/readme.txt":               "no deployments\n",
		"services/disappeared/compose.yaml":  "services: {}\n",
		"services/disappeared/.doco-cd.yaml": "context: nested-context\nswarm:\n  enabled: false\n",
	})

	previous, err := GetConfigsWithOrigins(t.Context(), source.artifact.Path, ".", "prod", "main",
		source.store.MirrorDir(), string(source.revision), source.gitOptions())
	if err != nil || len(previous.Configs) != 1 {
		t.Fatalf("previous resolution = %+v, error = %v; want one child", previous, err)
	}

	if err := os.Remove(filepath.Join(source.root, "services/disappeared/compose.yaml")); err != nil {
		t.Fatal(err)
	}

	source.commit(t, "pause empty roots", map[string]string{
		".doco-cd.prod.yaml": `working_dir: services
context: production
swarm:
  enabled: true
  config_retention: 2
  secret_retention: 3
pause: true
reconciliation: false
auto_discovery:
  enabled: true
  delete: true
---
working_dir: databases
context: development
swarm:
  enabled: false
pause: false
auto_discovery: true
`,
	})
	revision := source.resolve(t, "main")

	artifact, err := source.store.Publish(t.Context(), revision)
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := GetConfigsWithOrigins(t.Context(), artifact.Path, ".", "prod", "main",
		source.store.MirrorDir(), string(revision), source.gitOptions())
	if err != nil {
		t.Fatal(err)
	}

	if resolved.Configs == nil || len(resolved.Configs) != 0 {
		t.Fatalf("configs = %+v, want an explicit empty list with no synthetic deployments", resolved.Configs)
	}

	if len(resolved.Origins) != 2 {
		t.Fatalf("origins = %+v, want both configured roots", resolved.Origins)
	}

	for i, origin := range resolved.Origins {
		wantDir := []string{"services", "databases"}[i]
		wantContext := []string{"production", "development"}[i]

		wantPause := i == 0
		if origin.WorkingDirectory != wantDir || origin.Reference != "main" ||
			origin.Revision != string(revision) || origin.MirrorDir != source.store.MirrorDir() ||
			origin.Pause != wantPause || origin.Context != wantContext ||
			origin.ReconciliationEnabled != (i != 0) ||
			origin.Swarm.Enabled == nil || *origin.Swarm.Enabled != (i == 0) {
			t.Fatalf("origin %d = %+v, want resolved %s root with pause %+v", i, origin, wantDir, wantPause)
		}
	}

	if swarm := resolved.Origins[0].Swarm; swarm.ConfigRetention == nil || *swarm.ConfigRetention != 2 ||
		swarm.SecretRetention == nil || *swarm.SecretRetention != 3 {
		t.Fatalf("Swarm settings = %+v, want the configured retention settings", swarm)
	}

	if pause, found := PauseForAutoDiscoveryOrigin(previous.Configs[0], resolved.Origins); !found || pause != resolved.Origins[0].Pause {
		t.Fatalf("prior child of the now-empty root has pause %+v, found %v", pause, found)
	}
}

func TestGetConfigsWithOriginsUsesBasePauseBeforeNestedOverrides(t *testing.T) {
	t.Parallel()

	source := newDiscoveryTestSource(t, map[string]string{
		".doco-cd.yaml":       "auto_discovery: true\npause: true\n",
		"alpha/compose.yaml":  "services: {}\n",
		"alpha/.doco-cd.yaml": "pause: false\n",
		"beta/compose.yaml":   "services: {}\n",
		"beta/.doco-cd.yaml":  "pause: null\n",
	})

	resolved, err := GetConfigsWithOrigins(t.Context(), source.artifact.Path, ".", "", "main",
		source.store.MirrorDir(), string(source.revision), source.gitOptions())
	if err != nil {
		t.Fatal(err)
	}

	if len(resolved.Origins) != 1 || !resolved.Origins[0].Pause {
		t.Fatalf("origins = %+v, want the base deployment pause", resolved.Origins)
	}

	if names := configNames(resolved.Configs); !slices.Equal(names, []string{"alpha", "beta"}) {
		t.Fatalf("configs = %v, want both children", names)
	}

	for _, cfg := range resolved.Configs {
		wantPause := cfg.Name == "beta"
		if cfg.Pause != wantPause {
			t.Errorf("%s pause = %+v, want %+v", cfg.Name, cfg.Pause, wantPause)
		}

		if cfg.Internal.AutoDiscoveryOrigin == nil || *cfg.Internal.AutoDiscoveryOrigin != resolved.Origins[0] {
			t.Errorf("%s origin = %+v, want the unchanged base origin %+v",
				cfg.Name, cfg.Internal.AutoDiscoveryOrigin, resolved.Origins[0])
		}
	}
}

func TestResolveConfigsWithOriginsRetainsEmptyExternalSources(t *testing.T) {
	t.Parallel()

	source := newDiscoveryTestSource(t, map[string]string{".doco-cd.yaml": "auto_discovery: true\n"})
	source.checkout(t, "feature", true)
	feature := source.commit(t, "empty discovery root", map[string]string{"services/readme.txt": "empty\n"})
	source.checkout(t, "main", false)

	remote := newDiscoveryTestSource(t, map[string]string{"services/readme.txt": "empty\n"})
	opts := source.gitOptions()
	opts.SourceBaseDir = t.TempDir()
	bases := []*Config{
		{Reference: "feature", WorkingDirectory: "services", AutoDiscovery: AutoDiscoveryConfig{Enabled: true}, Pause: true},
		{RepositoryUrl: config.GitUrl(remote.root), Reference: "main", WorkingDirectory: "services", AutoDiscovery: AutoDiscoveryConfig{Enabled: true}},
	}

	resolved, err := ResolveConfigsWithOrigins(t.Context(), bases, "prod", "main", source.artifact.Path, ".",
		source.store.MirrorDir(), string(source.revision), opts)
	if err != nil {
		t.Fatal(err)
	}

	if len(resolved.Configs) != 0 || len(resolved.Origins) != len(bases) {
		t.Fatalf("resolved = %+v, want both empty discovery roots", resolved)
	}

	for i, origin := range resolved.Origins {
		base := bases[i]
		wantRevision := []string{feature.String(), string(remote.revision)}[i]

		wantMirror := []string{
			source.store.MirrorDir(),
			filepath.Join(remoteDiscoveryStoreDir(opts.SourceBaseDir, source.artifact.Path, remote.root), store.MirrorSubdir),
		}[i]
		if origin.Reference != base.Reference || origin.RepositoryURL != string(base.RepositoryUrl) ||
			origin.Revision != wantRevision || origin.MirrorDir != wantMirror || origin.Pause != base.Pause ||
			origin.WorkingDirectory != "services" {
			t.Fatalf("origin %d = %+v, want its own source metadata and pause", i, origin)
		}

		if base.Internal.AutoDiscoveryOrigin != nil || base.Internal.ConfigTarget != "" {
			t.Fatal("resolution mutated an inline discovery base")
		}
	}
}

func TestResolveConfigsWithOriginsRejectsMissingPausedRoot(t *testing.T) {
	t.Parallel()

	_, err := ResolveConfigsWithOrigins(t.Context(), []*Config{{
		WorkingDirectory: "missing", AutoDiscovery: AutoDiscoveryConfig{Enabled: true}, Pause: true,
	}}, "", "main", t.TempDir(), ".", "", "", nil)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing paused root error = %v, want the normal filesystem error", err)
	}
}
