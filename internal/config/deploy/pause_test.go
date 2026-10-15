package deploy

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/go-git/go-git/v5/plumbing"
	"go.yaml.in/yaml/v4"

	gitInternal "github.com/kimdre/doco-cd/internal/git"
)

func TestConfigPauseHashCompatibility(t *testing.T) {
	t.Parallel()

	cfg := New("app", "main")

	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(encoded), "pause:") {
		t.Fatalf("false pause must preserve the pre-pause YAML shape: %s", encoded)
	}

	want := fmt.Sprintf("%x", sha256.Sum256(encoded))

	for _, paused := range []bool{false, true} {
		cfg.Pause = paused

		got, err := cfg.Hash()
		if err != nil {
			t.Fatal(err)
		}

		if got != want {
			t.Fatalf("pause %v changed legacy config hash: got %s, want %s", paused, got, want)
		}

		if cfg.Pause != paused {
			t.Fatal("hashing mutated pause")
		}
	}
}

func TestConfigPauseNestedOverrides(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		base     bool
		override string
		want     bool
	}{
		{name: "omitted inherits", base: true, override: "environment: {LOG_LEVEL: debug}", want: true},
		{name: "false clears", base: true, override: "pause: false"},
		{name: "true sets", override: "pause: true", want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			base := New("app", "main")
			base.AutoDiscovery.Enabled = true
			base.Pause = test.base

			configs, err := autoDiscoverDeployments(fstest.MapFS{
				"service/compose.yaml": {Data: []byte("services: {}")},
				"service/.doco-cd.yml": {Data: []byte(test.override)},
			}, "pause-test", "", base)
			if err != nil {
				t.Fatal(err)
			}

			if len(configs) != 1 || configs[0].Pause != test.want {
				t.Fatalf("discovered pause = %+v, want %+v", configs, test.want)
			}

			if base.Pause != test.base {
				t.Fatal("nested pause override changed the base config")
			}
		})
	}
}

func TestConfigPauseDiscoveryCache(t *testing.T) {
	t.Parallel()

	source := newDiscoveryTestSource(t, map[string]string{
		"inherited/compose.yaml": "services: {}",
		"unpaused/compose.yaml":  "services: {}",
		"unpaused/.doco-cd.yaml": "pause: false",
	})

	tree, err := gitInternal.NewTreeFSAtCommit(source.repo, plumbing.NewHash(string(source.revision)))
	if err != nil {
		t.Fatal(err)
	}

	base := New("app", "main")
	base.AutoDiscovery.Enabled = true

	for i, pause := range []bool{true, false, true} {
		base.Pause = pause
		counted := &countingDiscoveryTreeFS{TreeFS: tree, reads: map[string]int{}}

		configs, err := autoDiscoverDeployments(counted, t.Name(), string(source.revision), base)
		if err != nil {
			t.Fatal(err)
		}

		if len(configs) != 2 {
			t.Fatalf("cached discovery must retain paused entries: got %d", len(configs))
		}

		if i > 0 && len(counted.reads) != 0 {
			t.Fatalf("base-only pause change must reuse the unchanged tree: %v", counted.reads)
		}

		want := map[string]bool{
			"inherited": pause,
			"unpaused":  false,
		}
		for _, cfg := range configs {
			if cfg.Pause != want[cfg.Name] {
				t.Fatalf("cached %s pause = %+v, want %+v", cfg.Name, cfg.Pause, want[cfg.Name])
			}

			cfg.Pause = !cfg.Pause
		}

		if base.Pause != pause {
			t.Fatal("mutating cached discovery results changed base pause")
		}
	}
}
