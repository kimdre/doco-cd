package docker

import (
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/compose"

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

// TestScaleDoesNotChangeServiceHash pins the compose behaviour the scale-out
// strategy depends on: setting scale to 2 must not make the service look
// diverged, or every poll would hand over again.
func TestScaleDoesNotChangeServiceHash(t *testing.T) {
	t.Parallel()

	svc := selfUpdateTestProject("doco-cd", "app").Services["app"]

	before, err := compose.ServiceHash(svc)
	if err != nil {
		t.Fatalf("hash before: %v", err)
	}

	two := 2
	svc.Scale = &two

	after, err := compose.ServiceHash(svc)
	if err != nil {
		t.Fatalf("hash after: %v", err)
	}

	if before != after {
		t.Errorf("scale changed the service hash: %s != %s", before, after)
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
