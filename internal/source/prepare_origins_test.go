package source

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestPrepareAutoDiscoveryOrigins(t *testing.T) {
	t.Parallel()

	srcPath, revision := createLocalGitFixture(t, map[string]string{
		".doco-cd.prod.yaml":  "working_dir: services\ncontext: production\nswarm:\n  enabled: true\nauto_discovery: true\npause: true\n",
		"services/readme.txt": "no compose files\n",
		"compose.yaml":        "services: {}\n",
	})

	for name, inline := range map[string][]*deploy.Config{
		"empty repository root": nil,
		"authoritative inline":  {{Name: "inline"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			result, err := newTestPreparer(t, nil).Prepare(t.Context(), Request{
				Logger: logger.New(logger.LevelCritical).Logger, JobTrigger: stages.JobTriggerWebhook,
				SourceType: config.SourceTypeGit, SourceRef: "file://" + srcPath, Ref: git.MainBranch,
				CustomTarget: "prod", Deployments: inline,
				Payload: webhook.ParsedPayload{Ref: git.MainBranch}, DataMountPoint: testMountPoint(t),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer result.Release()

			wantOrigins := 1
			if inline != nil {
				wantOrigins = 0
			}

			if len(result.DeployConfigs) != len(inline) || len(result.AutoDiscoveryOrigins) != wantOrigins {
				t.Fatalf("unexpected configs = %+v, origins = %+v", result.DeployConfigs, result.AutoDiscoveryOrigins)
			}

			if inline != nil {
				if cfg := result.DeployConfigs[0]; cfg.Name != "inline" || cfg.Pause != inline[0].Pause {
					t.Fatalf("repository pause contaminated inline config: %+v", cfg)
				}

				result.DeployConfigs[0].Pause = true
				if inline[0].Pause {
					t.Fatal("resolved config mutated the inline input")
				}

				return
			}

			origin := result.AutoDiscoveryOrigins[0]
			if origin.WorkingDirectory != "services" || origin.ConfigTarget != "prod" ||
				origin.Revision != revision || origin.MirrorDir != result.MirrorDir || !origin.Pause ||
				origin.Context != "production" || origin.Swarm.Enabled == nil || !*origin.Swarm.Enabled {
				t.Fatalf("origin = %+v, want the paused root and resolved source metadata", origin)
			}
		})
	}
}
