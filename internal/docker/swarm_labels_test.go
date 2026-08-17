package docker

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	composetypes "github.com/docker/cli/cli/compose/types"
	"github.com/go-git/go-git/v5/plumbing"
	swarmTypes "github.com/moby/moby/api/types/swarm"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// TestAddSwarmServiceLabels_UsesServiceLevelLabels verifies that the full deployment
// metadata is attached to the service and that only the subset that is stable between
// deployments is attached to the task template.
//
// Labels in the task template are part of the service definition, so writing the
// deployment timestamp there makes swarm recreate the tasks of every service on
// every deployment, even when nothing changed. Stable labels are safe and keep the
// containers identifiable on worker nodes.
func TestAddSwarmServiceLabels_UsesServiceLevelLabels(t *testing.T) {
	stack := &composetypes.Config{
		Services: []composetypes.ServiceConfig{
			{
				Name:   "web",
				Labels: composetypes.Labels{"user.label": "keep-me"},
				Deploy: composetypes.DeployConfig{
					Labels: composetypes.Labels{"user.deploy.label": "keep-me-too"},
				},
			},
			{
				Name: "worker",
			},
		},
	}

	deployConfig := &deploy.Config{Name: "test-stack"}
	deployConfig.Internal.ConfigTarget = "prod"
	deployConfig.Internal.Hash = "confighash"

	payload := &webhook.ParsedPayload{
		CommitSHA: plumbing.NewHash(strings.Repeat("a", 40)),
		FullName:  "kimdre/doco-cd_tests",
		WebURL:    "https://github.com/kimdre/doco-cd_tests",
	}

	addSwarmServiceLabels(stack, nil, deployConfig, payload, "/repo", "dev", "2026-01-01T00:00:00Z", "def456", "projecthash")

	// Labels that may differ between deployments of the same stack must never end up
	// in the task template. Source.URL is included because it differs between webhook
	// and poll triggers for the same repository.
	unstableLabels := []string{
		DocoCDLabels.Metadata.Version,
		DocoCDLabels.Deployment.Timestamp,
		DocoCDLabels.Deployment.ComposeHash,
		DocoCDLabels.Deployment.Trigger,
		DocoCDLabels.Deployment.CommitSHA,
		DocoCDLabels.Deployment.ConfigHash,
		DocoCDLabels.Deployment.AutoDiscovery,
		DocoCDLabels.Deployment.AutoDiscoveryConfig,
		DocoCDLabels.Source.URL,
	}

	// Stable labels keep the containers identifiable on worker nodes.
	stableLabels := []string{
		DocoCDLabels.Metadata.Manager,
		DocoCDLabels.Deployment.Name,
		DocoCDLabels.Deployment.WorkingDir,
		DocoCDLabels.Deployment.ConfigTarget,
		DocoCDLabels.Deployment.TargetRef,
		DocoCDLabels.Source.Type,
		DocoCDLabels.Source.Name,
	}

	metadataLabels := slices.Concat(stableLabels, unstableLabels)

	for _, service := range stack.Services {
		for _, label := range metadataLabels {
			if _, ok := service.Deploy.Labels[label]; !ok {
				t.Errorf("service %q: expected label %q in Deploy.Labels, got none", service.Name, label)
			}
		}

		for _, label := range stableLabels {
			if _, ok := service.Labels[label]; !ok {
				t.Errorf("service %q: expected stable label %q as a container label, got none", service.Name, label)
			}
		}

		// Exhaustiveness: any cd.doco.* container label not in the stable list is a
		// new addition to the task template and must be reviewed for stability.
		for label := range service.Labels {
			if strings.HasPrefix(label, "cd.doco.") && !slices.Contains(stableLabels, label) {
				t.Errorf("service %q: unexpected container label %q, add it to stableLabels only if it cannot change between deployments", service.Name, label)
			}
		}
	}

	if got := stack.Services[0].Labels["user.label"]; got != "keep-me" {
		t.Errorf("expected user defined container label to be preserved, got %q", got)
	}

	if got := stack.Services[0].Deploy.Labels["user.deploy.label"]; got != "keep-me-too" {
		t.Errorf("expected user defined service label to be preserved, got %q", got)
	}
}

// TestAddSwarmServiceLabels_ScopesCertLabelsPerService verifies that cert expiry/rotatable/state
// labels are only applied to the swarm services that actually consume a rotated certificate,
// mirroring the per-service scoping used for standalone Compose deployments.
func TestAddSwarmServiceLabels_ScopesCertLabelsPerService(t *testing.T) {
	certPEM := generateTestCertPEM(t, time.Now().Add(48*time.Hour).Truncate(time.Second))

	stack := &composetypes.Config{
		Services: []composetypes.ServiceConfig{
			{Name: "uses-cert"},
			{Name: "unrelated"},
		},
	}

	deployConfig := &deploy.Config{Name: "test-stack"}
	deployConfig.Internal.Environment = map[string]string{"CERT": certPEM}
	deployConfig.ExternalSecrets = map[string]secrettypes.ExternalSecretRef{
		"CERT": {LegacyRef: "pki-role:pki:my-role:app.example.com"},
	}

	unrelatedValue := "bar"
	project := &types.Project{
		Services: types.Services{
			"uses-cert": {
				Name:        "uses-cert",
				Environment: types.MappingWithEquals{"CERT": &certPEM},
			},
			"unrelated": {
				Name:        "unrelated",
				Environment: types.MappingWithEquals{"FOO": &unrelatedValue},
			},
		},
	}

	payload := &webhook.ParsedPayload{
		CommitSHA: plumbing.NewHash(strings.Repeat("a", 40)),
		FullName:  "kimdre/doco-cd_tests",
	}

	addSwarmServiceLabels(stack, project, deployConfig, payload, "/repo", "dev", "2026-01-01T00:00:00Z", "def456", "projecthash")

	byName := make(map[string]composetypes.ServiceConfig, len(stack.Services))
	for _, s := range stack.Services {
		byName[s.Name] = s
	}

	if _, ok := byName["uses-cert"].Deploy.Labels[DocoCDLabels.Deployment.CertRotatable]; !ok {
		t.Errorf("expected certificate-consuming service to carry %q", DocoCDLabels.Deployment.CertRotatable)
	}

	if _, ok := byName["uses-cert"].Deploy.Labels[DocoCDLabels.Deployment.CertState]; !ok {
		t.Errorf("expected certificate-consuming service to carry %q", DocoCDLabels.Deployment.CertState)
	}

	for _, label := range []string{DocoCDLabels.Deployment.CertRotatable, DocoCDLabels.Deployment.CertExpiry, DocoCDLabels.Deployment.CertState} {
		if _, ok := byName["unrelated"].Deploy.Labels[label]; ok {
			t.Errorf("expected unrelated service to not carry %q", label)
		}
	}
}

// TestAddSwarmVolumeLabels_OmitsUnstableLabels verifies that volumes are not labeled
// with deployment metadata that changes on every deployment.
//
// Volume labels are converted into the mount options of the task template, so labels
// that change on every deployment would recreate the tasks of every service that uses
// a volume.
func TestAddSwarmVolumeLabels_OmitsUnstableLabels(t *testing.T) {
	stack := &composetypes.Config{
		Volumes: map[string]composetypes.VolumeConfig{
			"data": {Name: "data"},
		},
	}

	deployConfig := &deploy.Config{Name: "test-stack"}
	payload := &webhook.ParsedPayload{CommitSHA: plumbing.NewHash(strings.Repeat("a", 40)), FullName: "kimdre/doco-cd_tests"}

	addSwarmVolumeLabels(stack, deployConfig, payload, "/repo")

	labels := stack.Volumes["data"].Labels
	if len(labels) == 0 {
		t.Fatal("expected the volume to be labeled")
	}

	for _, label := range []string{
		DocoCDLabels.Deployment.Timestamp,
		DocoCDLabels.Deployment.CommitSHA,
		DocoCDLabels.Deployment.Trigger,
		DocoCDLabels.Metadata.Version,
		DocoCDLabels.Source.URL,
	} {
		if _, ok := labels[label]; ok {
			t.Errorf("label %q must not be set on volumes, it changes between deployments", label)
		}
	}
}

func TestSwarmServiceLabels(t *testing.T) {
	testCases := []struct {
		name    string
		service swarmTypes.Service
		want    Labels
	}{
		{
			name: "service spec labels",
			service: swarmTypes.Service{
				Spec: swarmTypes.ServiceSpec{
					Annotations: swarmTypes.Annotations{
						Labels: map[string]string{DocoCDLabels.Deployment.Name: "stack"},
					},
					TaskTemplate: swarmTypes.TaskSpec{ContainerSpec: &swarmTypes.ContainerSpec{}},
				},
			},
			want: Labels{DocoCDLabels.Deployment.Name: "stack"},
		},
		{
			name: "container spec labels of a stack deployed by an earlier version",
			service: swarmTypes.Service{
				Spec: swarmTypes.ServiceSpec{
					TaskTemplate: swarmTypes.TaskSpec{
						ContainerSpec: &swarmTypes.ContainerSpec{
							Labels: map[string]string{DocoCDLabels.Deployment.Name: "legacy"},
						},
					},
				},
			},
			want: Labels{DocoCDLabels.Deployment.Name: "legacy"},
		},
		{
			name: "service spec labels take precedence",
			service: swarmTypes.Service{
				Spec: swarmTypes.ServiceSpec{
					Annotations: swarmTypes.Annotations{
						Labels: map[string]string{DocoCDLabels.Deployment.Timestamp: "new"},
					},
					TaskTemplate: swarmTypes.TaskSpec{
						ContainerSpec: &swarmTypes.ContainerSpec{
							Labels: map[string]string{
								DocoCDLabels.Deployment.Timestamp: "old",
								DocoCDJobLabels.JobEnabled:        "true",
							},
						},
					},
				},
			},
			want: Labels{
				DocoCDLabels.Deployment.Timestamp: "new",
				DocoCDJobLabels.JobEnabled:        "true",
			},
		},
		{
			name: "service without a container spec",
			service: swarmTypes.Service{
				Spec: swarmTypes.ServiceSpec{
					Annotations: swarmTypes.Annotations{
						Labels: map[string]string{DocoCDLabels.Metadata.Manager: "doco-cd"},
					},
				},
			},
			want: Labels{DocoCDLabels.Metadata.Manager: "doco-cd"},
		},
		{
			name:    "service without any labels",
			service: swarmTypes.Service{},
			want:    Labels{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := SwarmServiceLabels(tc.service)

			if len(got) != len(tc.want) {
				t.Fatalf("expected %d labels, got %d: %v", len(tc.want), len(got), got)
			}

			for key, want := range tc.want {
				if got[key] != want {
					t.Errorf("label %q: expected %q, got %q", key, want, got[key])
				}
			}
		})
	}
}

// TestSwarmJobLabels verifies that job configuration labels are only honored when
// they are part of the task template, which is where the deploy path reads them from,
// while job runtime metadata written by doco-cd to the service spec is kept.
func TestSwarmJobLabels(t *testing.T) {
	service := swarmTypes.Service{
		Spec: swarmTypes.ServiceSpec{
			Annotations: swarmTypes.Annotations{
				Labels: map[string]string{
					DocoCDLabels.Deployment.Name: "stack",
					DocoCDJobLabels.JobEnabled:   "true",
					DocoCDJobLabels.JobSchedule:  "@every 1h",
					DocoCDJobLabels.JobLastRun:   "2026-01-01T00:00:00Z",
				},
			},
			TaskTemplate: swarmTypes.TaskSpec{
				ContainerSpec: &swarmTypes.ContainerSpec{
					Labels: map[string]string{DocoCDJobLabels.JobSchedule: "@every 5m"},
				},
			},
		},
	}

	labels := SwarmJobLabels(service)

	if _, ok := labels[DocoCDJobLabels.JobEnabled]; ok {
		t.Error("expected job config labels that are only set on the service spec to be ignored")
	}

	if got := labels[DocoCDJobLabels.JobSchedule]; got != "@every 5m" {
		t.Errorf("expected the job config label of the task template to be used, got %q", got)
	}

	if got := labels[DocoCDLabels.Deployment.Name]; got != "stack" {
		t.Errorf("expected deployment metadata to be kept, got %q", got)
	}

	if got := labels[DocoCDJobLabels.JobLastRun]; got != "2026-01-01T00:00:00Z" {
		t.Errorf("expected job runtime metadata of the service spec to be kept, got %q", got)
	}
}
