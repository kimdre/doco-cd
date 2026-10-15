package deploy

import "testing"

func TestPauseForAutoDiscoveryOrigin(t *testing.T) {
	t.Parallel()

	compose, swarm := false, true
	base := AutoDiscoveryOrigin{
		WorkingDirectory: "./services/", Reference: "refs/heads/main", ConfigTarget: "prod", Context: "production",
		RepositoryURL: "https://example.com/organization/repo.git",
		Revision:      "old-revision", MirrorDir: "/old/mirror", Swarm: SwarmConfig{Enabled: &compose},
		Settings: AutoDiscoveryConfig{Enabled: true, ScanDepth: 1}, Pause: true,
	}

	for _, tt := range []struct {
		name      string
		change    func(*AutoDiscoveryOrigin, *AutoDiscoveryOrigin)
		wantFound bool
		wantPause bool
	}{
		{name: "equivalent owner ignores revision mode and child overrides", wantFound: true, wantPause: true},
		{
			name: "legacy origins inherit the deployment target",
			change: func(previous, _ *AutoDiscoveryOrigin) {
				previous.ConfigTarget = ""
			},
			wantFound: true, wantPause: true,
		},
		{
			name: "default context aliases are equivalent",
			change: func(previous, root *AutoDiscoveryOrigin) {
				previous.Context, root.Context = "", " DEFAULT "
			},
			wantFound: true, wantPause: true,
		},
		{
			name: "an explicitly unpaused root clears prior root pause",
			change: func(_, root *AutoDiscoveryOrigin) {
				root.Pause = false
			},
			wantFound: true,
		},
		{name: "unrelated root", change: func(_, root *AutoDiscoveryOrigin) { root.WorkingDirectory = "databases" }},
		{name: "overlapping root is not the same root", change: func(_, root *AutoDiscoveryOrigin) { root.WorkingDirectory = "." }},
		{name: "unrelated source", change: func(_, root *AutoDiscoveryOrigin) { root.RepositoryURL = "https://example.com/other/repo.git" }},
		{name: "primary source is not remote", change: func(_, root *AutoDiscoveryOrigin) { root.RepositoryURL = "" }},
		{name: "unrelated reference", change: func(_, root *AutoDiscoveryOrigin) { root.Reference = "feature" }},
		{name: "unrelated target", change: func(_, root *AutoDiscoveryOrigin) { root.ConfigTarget = "dev" }},
		{name: "root context remains authoritative over child context", change: func(_, root *AutoDiscoveryOrigin) { root.Context = "nested-context" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			previous, current := base, base
			current.WorkingDirectory, current.Reference, current.ConfigTarget = "services", "main", "prod"
			current.RepositoryURL = "git@example.com:organization/repo"
			current.Revision, current.MirrorDir, current.WebhookEventFilter = "new-revision", "/new/mirror", "^main$"
			current.Swarm.Enabled, current.Settings.ScanDepth, current.Settings.Delete = &swarm, 2, true
			dc := &Config{
				Name: "disappeared", WorkingDirectory: "elsewhere", Context: "nested-context",
				Swarm: SwarmConfig{Enabled: &compose},
			}

			dc.Internal.ConfigTarget, dc.Internal.AutoDiscoveryOrigin = "prod", &previous
			if tt.change != nil {
				tt.change(&previous, &current)
			}

			pause, found := PauseForAutoDiscoveryOrigin(dc, []AutoDiscoveryOrigin{current})
			if found != tt.wantFound || pause != tt.wantPause {
				t.Fatalf("PauseForAutoDiscoveryOrigin() = %+v, %v; want %+v, %v", pause, found, tt.wantPause, tt.wantFound)
			}
		})
	}
}

func TestPauseForAutoDiscoveryOriginMissingOrRepeatedRoots(t *testing.T) {
	t.Parallel()

	root := AutoDiscoveryOrigin{WorkingDirectory: "services", Reference: "main", Pause: true}
	for _, dc := range []*Config{nil, {Name: "explicit"}} {
		if pause, found := PauseForAutoDiscoveryOrigin(dc, []AutoDiscoveryOrigin{root}); found || pause {
			t.Fatalf("config without provenance matched a root: %+v, %v", pause, found)
		}
	}

	dc := &Config{Name: "prior"}

	dc.Internal.AutoDiscoveryOrigin = &root
	if pause, found := PauseForAutoDiscoveryOrigin(dc, nil); found || pause {
		t.Fatalf("missing current root produced a pause: %+v, %v", pause, found)
	}

	second := root

	second.Pause = false
	for _, roots := range [][]AutoDiscoveryOrigin{{root, second}, {second, root}} {
		pause, found := PauseForAutoDiscoveryOrigin(dc, roots)
		if !found || !pause {
			t.Fatalf("matching paused root was ignored: %v, %v", pause, found)
		}
	}
}
