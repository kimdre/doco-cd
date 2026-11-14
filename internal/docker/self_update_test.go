package docker

import (
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/kimdre/doco-cd/internal/common/types/duration"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

func selfUpdateTestProject(name, service string) *types.Project {
	one := 1

	return &types.Project{
		Name: name,
		Services: types.Services{
			service: {Name: service, Image: "ghcr.io/kimdre/doco-cd:1.2.3", Restart: "unless-stopped", Scale: &one},
			"other": {Name: "other", Image: "nginx:1.29"},
		},
	}
}

func withSelfIdentity(t *testing.T, project, service string) {
	t.Helper()

	configureTestSelfIdentity(t, project, service, false)
}

func withSwarmSelfIdentity(t *testing.T, project, service string) {
	t.Helper()

	configureTestSelfIdentity(t, project, service, true)
}

func configureTestSelfIdentity(t *testing.T, project, service string, swarm bool) {
	t.Helper()

	previous := SelfUpdateConfig()

	t.Cleanup(func() { ConfigureSelfUpdate(previous) })

	ConfigureSelfUpdate(SelfUpdateOptions{
		Enabled: true,
		Identity: selfupdate.Identity{
			OK:            project != "",
			ContainerID:   "abc123",
			ContainerName: "doco-cd",
			Project:       project,
			Service:       service,
			Number:        1,
			Swarm:         swarm,
		},
	})
}

func TestSelfUpdateFor(t *testing.T) {
	tests := []struct {
		name            string
		identityProject string
		identityService string
		projectName     string
		contextName     string
		want            bool
	}{
		{name: "project and service match", identityProject: "doco-cd", identityService: "app", projectName: "doco-cd", want: true},
		{name: "project name is case insensitive", identityProject: "Doco-CD", identityService: "app", projectName: "doco-cd", want: true},
		{name: "default context spelled out", identityProject: "doco-cd", identityService: "app", projectName: "doco-cd", contextName: "default", want: true},
		{name: "another project", identityProject: "doco-cd", identityService: "app", projectName: "something-else"},
		{name: "remote context", identityProject: "doco-cd", identityService: "app", projectName: "doco-cd", contextName: "remote"},
		{name: "service is not in the project", identityProject: "doco-cd", identityService: "missing", projectName: "doco-cd"},
		{name: "no compose identity", projectName: "doco-cd"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withSelfIdentity(t, tt.identityProject, tt.identityService)

			got := selfUpdateFor(selfUpdateTestProject(tt.projectName, "app"), tt.contextName)

			if (got != nil) != tt.want {
				t.Errorf("selfUpdateFor = %v, want match %v", got, tt.want)
			}
		})
	}
}

func TestSelfUpdateForSwarm(t *testing.T) {
	withSwarmSelfIdentity(t, "doco-cd", "app")

	if got := selfUpdateFor(selfUpdateTestProject("doco-cd", "app"), ""); got != nil {
		t.Errorf("selfUpdateFor = %v, want nil for a swarm identity", got)
	}
}

func TestSwarmSelfService(t *testing.T) {
	tests := []struct {
		name  string
		swarm bool
		dc    *deploy.Config
		want  string
	}{
		{name: "own swarm stack", swarm: true, dc: &deploy.Config{Name: "doco-cd"}, want: "doco-cd_app"},
		{name: "explicit default context", swarm: true, dc: &deploy.Config{Name: "doco-cd", Context: "default"}, want: "doco-cd_app"},
		{name: "case sensitive swarm stack", swarm: true, dc: &deploy.Config{Name: "Doco-CD"}},
		{name: "another swarm stack", swarm: true, dc: &deploy.Config{Name: "my-app"}},
		{name: "remote swarm stack", swarm: true, dc: &deploy.Config{Name: "doco-cd", Context: "remote"}},
		{name: "own compose stack", dc: &deploy.Config{Name: "doco-cd"}},
		{name: "no config", swarm: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configureTestSelfIdentity(t, "doco-cd", "app", tt.swarm)

			if got := swarmSelfService(tt.dc); got != tt.want {
				t.Errorf("swarmSelfService = %q, want %q", got, tt.want)
			}

			if tt.swarm && IsSelfStack(tt.dc) != (tt.want != "") {
				t.Errorf("IsSelfStack does not match the selected self-service")
			}
		})
	}
}

func TestIsSelfStack(t *testing.T) {
	withSelfIdentity(t, "doco-cd", "app")

	tests := []struct {
		name string
		dc   *deploy.Config
		want bool
	}{
		{name: "same stack", dc: &deploy.Config{Name: "doco-cd"}, want: true},
		{name: "another stack", dc: &deploy.Config{Name: "my-app"}},
		{name: "remote context", dc: &deploy.Config{Name: "doco-cd", Context: "remote"}},
		{name: "no config"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSelfStack(tt.dc); got != tt.want {
				t.Errorf("IsSelfStack = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSelfHealthTimeout(t *testing.T) {
	t.Parallel()

	record := selfupdate.Record{Deploy: selfupdate.DeployInfo{TimeoutSeconds: 60}}

	for _, test := range []struct {
		name string
		cfg  *deploy.Config
		want time.Duration
	}{
		{name: "nil config", want: time.Minute},
		{name: "unset timeout", cfg: &deploy.Config{}, want: time.Minute},
		{name: "configured timeout", cfg: &deploy.Config{Timeout: duration.Duration(90 * time.Second)}, want: 90 * time.Second},
		{name: "negative timeout", cfg: &deploy.Config{Timeout: duration.Duration(-time.Second)}, want: time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := selfHealthTimeout(record, test.cfg); got != test.want {
				t.Fatalf("health timeout = %s, want %s", got, test.want)
			}
		})
	}
}

func TestSelfDeployInputFromLabels(t *testing.T) {
	t.Parallel()

	if got := SelfDeployInputFromLabels(nil); got != nil {
		t.Errorf("empty labels returned %v, want nil", got)
	}

	got := SelfDeployInputFromLabels(map[string]string{
		DocoCDLabels.Source.Name:            " gitserver/infra ",
		DocoCDLabels.Source.URL:             "https://example.com/infra.git",
		DocoCDLabels.Source.Type:            "git",
		DocoCDLabels.Deployment.TargetRef:   "refs/heads/main",
		DocoCDLabels.Deployment.CommitSHA:   "abc123",
		DocoCDLabels.Deployment.ComposeHash: "hash",
	})

	if got.RepoName != "gitserver/infra" {
		t.Errorf("RepoName = %q, want the trimmed label", got.RepoName)
	}

	if got.CommitSHA != "abc123" || got.ProjectHash != "hash" || got.Reference != "refs/heads/main" {
		t.Errorf("deploy input lost label data: %+v", got)
	}
}

// TestSelfDeployInputCarriesCommitStatus checks that the pending commit status
// of a deployment reaches the journal, and that label-based redeploys, which
// post none, do not hand one over.
func TestSelfDeployInputCarriesCommitStatus(t *testing.T) {
	t.Parallel()

	status := &selfupdate.CommitStatusInfo{
		SourceURL: "https://git.example.com/org/infra.git",
		CommitSHA: "abc123",
		Context:   "doco-cd/nas/doco-cd",
	}

	req := runtimeDeployRequest{request: DeployRequest{
		DeployConfig: &deploy.Config{Name: "doco-cd", Timeout: duration.Duration(90 * time.Second)},
		LatestCommit: "abc123",
		CommitStatus: status,
	}}

	in := req.selfDeployInput()
	if in.CommitStatus != status {
		t.Fatalf("selfDeployInput().CommitStatus = %v, want %v", in.CommitStatus, status)
	}

	if in.TimeoutSeconds != 90 {
		t.Fatalf("selfDeployInput().TimeoutSeconds = %d, want 90", in.TimeoutSeconds)
	}

	if got := selfSourceInfo(in).CommitStatus; got != status {
		t.Errorf("selfSourceInfo().CommitStatus = %v, want %v", got, status)
	}

	fromLabels := SelfDeployInputFromLabels(map[string]string{DocoCDLabels.Deployment.CommitSHA: "abc123"})
	if got := selfSourceInfo(fromLabels).CommitStatus; got != nil {
		t.Errorf("label-based redeploy handed over commit status %+v, want nil", *got)
	}
}
