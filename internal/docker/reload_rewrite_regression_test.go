package docker

import (
	"sync"
	"testing"

	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestReloadDeploymentCacheWithRewrittenSource(t *testing.T) {
	for _, source := range []string{
		"https://forgejo.example.com/org/repo.git",
		"ssh://git@forgejo:2222/org/repo.git",
		"ssh://git@vpn-forgejo.internal:2222/org/repo.git",
	} {
		t.Run(source, func(t *testing.T) {
			cache := &sync.Map{}
			payload := &webhook.ParsedPayload{
				CloneURL: "https://forgejo.example.com/org/repo.git",
			}
			// DeployStack must record B using the same active identity as Stage 2.
			key := deploymentStateRepositoryKey(DeployRequest{
				Payload: payload, RepositoryName: git.GetRepoName(source),
			})
			cache.Store(getDeployStatusCacheKey(key, "proxy"), deployStatus{
				CommitSHA: "B", ComposeHash: "unchanged-compose",
			})
			// The unrecreated container still carries initial revision A.
			services := map[Service]ServiceStatus{
				"caddy": {Labels: Labels{
					DocoCDLabels.Source.Name:            "org/repo",
					DocoCDLabels.Deployment.CommitSHA:   "A",
					DocoCDLabels.Deployment.ComposeHash: "unchanged-compose",
				}},
			}
			// Stage 2 looks up using Repository.Name from the rewritten source.
			status := getLatestServiceStatus(cache, services, git.GetRepoName(source), "proxy")
			if got := status.GetDeploymentCommitSHA(); got != "B" {
				t.Fatalf("successful reload B lost: lookup returned %q; write key=%q, read key=%q", got, key, git.GetRepoName(source))
			}
		})
	}
}

func TestDeploymentStateRepositoryKey(t *testing.T) {
	payload := &webhook.ParsedPayload{CloneURL: "https://public.example/org/config.git"}
	for _, tc := range []struct {
		name string
		req  DeployRequest
		want string
	}{
		{
			name: "rewritten source",
			req:  DeployRequest{Payload: payload, RepositoryName: "internal.example:2222/org/config"},
			want: "internal.example:2222/org/config",
		},
		{
			name: "repository override differs from configuration source",
			req:  DeployRequest{Payload: payload, SourceURL: "ssh://git@internal.example:2222/org/config.git", RepositoryName: "deploy.example/org/application"},
			want: "deploy.example/org/application",
		},
		{
			name: "legacy direct caller",
			req:  DeployRequest{Payload: payload},
			want: "public.example/org/config",
		},
		{
			name: "empty identity falls back to payload",
			req:  DeployRequest{Payload: payload, RepositoryName: "  "},
			want: "public.example/org/config",
		},
		{
			name: "explicit identity without webhook",
			req:  DeployRequest{RepositoryName: " internal.example/org/config "},
			want: "internal.example/org/config",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := deploymentStateRepositoryKey(tc.req); got != tc.want {
				t.Fatalf("cache repository = %q, want %q", got, tc.want)
			}
		})
	}
}
