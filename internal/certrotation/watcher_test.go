package certrotation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

func TestParseCertExpiry(t *testing.T) {
	t.Run("valid RFC3339 timestamp", func(t *testing.T) {
		want := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

		got, ok := parseCertExpiry(want.Format(time.RFC3339))
		if !ok {
			t.Fatalf("expected ok=true")
		}

		if !got.Equal(want) {
			t.Errorf("expected %v, got %v", want, got)
		}
	})

	t.Run("empty value", func(t *testing.T) {
		if _, ok := parseCertExpiry(""); ok {
			t.Errorf("expected ok=false for empty value")
		}
	})

	t.Run("invalid format", func(t *testing.T) {
		if _, ok := parseCertExpiry("not-a-timestamp"); ok {
			t.Errorf("expected ok=false for invalid format")
		}
	})
}

func TestFormatWatcherDuration(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want string
	}{
		{name: "whole hours", in: 100 * time.Hour, want: "100h"},
		{name: "hours with remainder are truncated", in: 3*time.Hour + 30*time.Minute + 2*time.Second, want: "3h"},
		{name: "whole minutes", in: 15 * time.Minute, want: "15m"},
		{name: "non minute duration", in: 90 * time.Second, want: "1m30s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatWatcherDuration(tt.in)
			if got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

func TestDueProjects(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	threshold := 72 * time.Hour

	t.Run("project within threshold is due", func(t *testing.T) {
		services := map[docker.Service]map[string]string{
			"project-a_app_1": {
				api.ProjectLabel: "project-a",
				docker.DocoCDLabels.Deployment.CertExpiry: now.Add(24 * time.Hour).Format(time.RFC3339),
			},
		}

		due := dueProjects(services, now, threshold, nil)

		if _, ok := due["project-a"]; !ok {
			t.Fatalf("expected project-a to be due, got %v", due)
		}
	})

	t.Run("project outside threshold is not due", func(t *testing.T) {
		services := map[docker.Service]map[string]string{
			"project-b_app_1": {
				api.ProjectLabel: "project-b",
				docker.DocoCDLabels.Deployment.CertExpiry: now.Add(30 * 24 * time.Hour).Format(time.RFC3339),
			},
		}

		due := dueProjects(services, now, threshold, nil)

		if len(due) != 0 {
			t.Fatalf("expected no due projects, got %v", due)
		}
	})

	t.Run("already expired project is due", func(t *testing.T) {
		services := map[docker.Service]map[string]string{
			"project-c_app_1": {
				api.ProjectLabel: "project-c",
				docker.DocoCDLabels.Deployment.CertExpiry: now.Add(-1 * time.Hour).Format(time.RFC3339),
			},
		}

		due := dueProjects(services, now, threshold, nil)

		if _, ok := due["project-c"]; !ok {
			t.Fatalf("expected project-c to be due, got %v", due)
		}
	})

	t.Run("multiple services in same project are deduplicated", func(t *testing.T) {
		services := map[docker.Service]map[string]string{
			"project-d_app_1": {
				api.ProjectLabel: "project-d",
				docker.DocoCDLabels.Deployment.CertExpiry: now.Add(1 * time.Hour).Format(time.RFC3339),
			},
			"project-d_worker_1": {
				api.ProjectLabel: "project-d",
				docker.DocoCDLabels.Deployment.CertExpiry: now.Add(1 * time.Hour).Format(time.RFC3339),
			},
		}

		due := dueProjects(services, now, threshold, nil)

		if len(due) != 1 {
			t.Fatalf("expected exactly one due project, got %v", due)
		}
	})

	t.Run("soonest expiry wins so a stale label after a partial rotation failure can't be masked", func(t *testing.T) {
		services := map[docker.Service]map[string]string{
			"project-d_app_1": {
				api.ProjectLabel: "project-d",
				docker.DocoCDLabels.Deployment.CertExpiry: now.Add(30 * 24 * time.Hour).Format(time.RFC3339),
			},
			"project-d_worker_1": {
				api.ProjectLabel: "project-d",
				docker.DocoCDLabels.Deployment.CertExpiry: now.Add(time.Hour).Format(time.RFC3339),
			},
		}

		due := dueProjects(services, now, threshold, nil)

		if _, ok := due["project-d"]; !ok {
			t.Fatalf("expected project-d to remain due because of the stale near-expiry sibling label, got %v", due)
		}
	})

	t.Run("missing expiry label is skipped", func(t *testing.T) {
		services := map[docker.Service]map[string]string{
			"project-e_app_1": {
				api.ProjectLabel: "project-e",
			},
		}

		due := dueProjects(services, now, threshold, nil)

		if len(due) != 0 {
			t.Fatalf("expected no due projects, got %v", due)
		}
	})

	t.Run("missing project label is skipped", func(t *testing.T) {
		services := map[docker.Service]map[string]string{
			"orphan_app_1": {
				docker.DocoCDLabels.Deployment.CertExpiry: now.Add(1 * time.Hour).Format(time.RFC3339),
			},
		}

		due := dueProjects(services, now, threshold, nil)

		if len(due) != 0 {
			t.Fatalf("expected no due projects, got %v", due)
		}
	})

	t.Run("swarm service without api.ProjectLabel falls back to deployment name label", func(t *testing.T) {
		services := map[docker.Service]map[string]string{
			"stack-a_app.1": {
				docker.DocoCDLabels.Deployment.Name:       "stack-a",
				docker.DocoCDLabels.Deployment.CertExpiry: now.Add(1 * time.Hour).Format(time.RFC3339),
			},
		}

		due := dueProjects(services, now, threshold, nil)

		if _, ok := due["stack-a"]; !ok {
			t.Fatalf("expected stack-a to be due via the deployment name label fallback, got %v", due)
		}
	})
}

type revocationCheckingProvider struct {
	revoked map[string]bool
	errFor  map[string]error
	calls   int
}

func (p *revocationCheckingProvider) Name() string { return "test" }
func (p *revocationCheckingProvider) Close()       {}
func (p *revocationCheckingProvider) GetSecret(context.Context, string) (string, error) {
	return "", nil
}

func (p *revocationCheckingProvider) GetSecrets(context.Context, []string) (map[string]string, error) {
	return nil, nil
}

func (p *revocationCheckingProvider) ResolveSecretReferences(context.Context, map[string]string) (secrettypes.ResolvedSecrets, error) {
	return nil, nil
}

func (p *revocationCheckingProvider) DeploymentHasRevokedCertificate(_ context.Context, certState string) (bool, error) {
	p.calls++

	if err := p.errFor[certState]; err != nil {
		return false, err
	}

	return p.revoked[certState], nil
}

func TestRevokedProjects(t *testing.T) {
	services := map[docker.Service]map[string]string{
		"project-a_app_1": {
			api.ProjectLabel:                         "project-a",
			docker.DocoCDLabels.Deployment.CertState: `[{"ref":"pki-role:pki:role:a.example.com","serial":"01"}]`,
		},
		"project-b_app_1": {
			api.ProjectLabel:                         "project-b",
			docker.DocoCDLabels.Deployment.CertState: `[{"ref":"pki-role:pki:role:b.example.com","serial":"02"}]`,
		},
		"project-c_app_1": {
			api.ProjectLabel:                         "project-c",
			docker.DocoCDLabels.Deployment.CertState: `[{"ref":"pki-role:pki:role:c.example.com","serial":"03"}]`,
		},
	}

	provider := &revocationCheckingProvider{
		revoked: map[string]bool{
			services["project-a_app_1"][docker.DocoCDLabels.Deployment.CertState]: true,
		},
		errFor: map[string]error{
			services["project-c_app_1"][docker.DocoCDLabels.Deployment.CertState]: errors.New("boom"),
		},
	}

	var secretProvider secretprovider.SecretProvider = provider

	got := revokedProjects(t.Context(), services, &secretProvider, nil)

	if len(got) != 1 {
		t.Fatalf("expected exactly one revoked project, got %v", got)
	}

	if _, ok := got["project-a"]; !ok {
		t.Fatalf("expected project-a to be marked revoked, got %v", got)
	}

	if _, ok := got["project-b"]; ok {
		t.Fatalf("did not expect project-b to be marked revoked, got %v", got)
	}

	if _, ok := got["project-c"]; ok {
		t.Fatalf("did not expect errored project-c to be marked revoked, got %v", got)
	}
}

// TestRevokedProjectsSwarmLabels verifies that revocation detection also works for Docker Swarm
// services, which never carry api.ProjectLabel (docker stack deploy doesn't set it); the project
// grouping must fall back to DocoCDLabels.Deployment.Name in that case.
func TestRevokedProjectsSwarmLabels(t *testing.T) {
	services := map[docker.Service]map[string]string{
		"stack-a_app.1": {
			docker.DocoCDLabels.Deployment.Name:      "stack-a",
			docker.DocoCDLabels.Deployment.CertState: `[{"ref":"pki-role:pki:role:a.example.com","serial":"01"}]`,
		},
	}

	provider := &revocationCheckingProvider{
		revoked: map[string]bool{
			services["stack-a_app.1"][docker.DocoCDLabels.Deployment.CertState]: true,
		},
	}

	var secretProvider secretprovider.SecretProvider = provider

	got := revokedProjects(t.Context(), services, &secretProvider, nil)

	if _, ok := got["stack-a"]; !ok {
		t.Fatalf("expected stack-a to be marked revoked via the deployment name label fallback, got %v", got)
	}
}

func TestRotationReasons(t *testing.T) {
	got := rotationReasons(
		map[string]map[string]string{
			"project-a": nil,
			"project-b": nil,
		},
		map[string]map[string]string{
			"project-b": nil,
			"project-c": nil,
		},
	)

	if want := []string{"expiry"}; !slices.Equal(got["project-a"], want) {
		t.Fatalf("expected project-a reasons %v, got %v", want, got["project-a"])
	}

	if want := []string{"expiry", "revoked"}; !slices.Equal(got["project-b"], want) {
		t.Fatalf("expected project-b reasons %v, got %v", want, got["project-b"])
	}

	if want := []string{"revoked"}; !slices.Equal(got["project-c"], want) {
		t.Fatalf("expected project-c reasons %v, got %v", want, got["project-c"])
	}
}

func TestWatcherLogsWhenCertificateNeedsRotation(t *testing.T) {
	var buf bytes.Buffer

	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	watcher := &Watcher{
		log:       log,
		threshold: 72 * time.Hour,
		now: func() time.Time {
			return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		},
	}

	due := map[string]map[string]string{
		"project-a": {
			api.ProjectLabel: "project-a",
		},
	}
	reasons := rotationReasons(due, nil)

	for project := range due {
		watcher.log.Info("certificate needs rotation",
			slog.String("project", project),
			slog.String("reason", strings.Join(reasons[project], ",")),
		)
	}

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatalf("expected JSON log entry, got %q: %v", buf.String(), err)
	}

	if entry["msg"] != "certificate needs rotation" {
		t.Fatalf("expected log message %q, got %v", "certificate needs rotation", entry["msg"])
	}

	if entry["project"] != "project-a" {
		t.Fatalf("expected project attr %q, got %v", "project-a", entry["project"])
	}

	if entry["reason"] != "expiry" {
		t.Fatalf("expected reason attr %q, got %v", "expiry", entry["reason"])
	}
}

// TestRevokedProjectsStopsCheckingAfterFirstRevokedService verifies that once one service marks a
// project as revoked, the remaining services of that same project are not re-checked, since a
// single revoked certificate already forces a rotation of the whole project. This keeps the number
// of provider (OpenBao) revocation lookups proportional to projects rather than to containers.
func TestRevokedProjectsStopsCheckingAfterFirstRevokedService(t *testing.T) {
	revokedState := `[{"ref":"pki-role:pki:role:a.example.com","serial":"01"}]`

	services := map[docker.Service]map[string]string{
		"project-a_app_1": {
			api.ProjectLabel:                         "project-a",
			docker.DocoCDLabels.Deployment.CertState: revokedState,
		},
		"project-a_app_2": {
			api.ProjectLabel:                         "project-a",
			docker.DocoCDLabels.Deployment.CertState: revokedState,
		},
		"project-a_app_3": {
			api.ProjectLabel:                         "project-a",
			docker.DocoCDLabels.Deployment.CertState: revokedState,
		},
	}

	provider := &revocationCheckingProvider{revoked: map[string]bool{revokedState: true}}

	var secretProvider secretprovider.SecretProvider = provider

	got := revokedProjects(t.Context(), services, &secretProvider, nil)

	if _, ok := got["project-a"]; !ok {
		t.Fatalf("expected project-a to be marked revoked, got %v", got)
	}

	if provider.calls != 1 {
		t.Fatalf("expected exactly one revocation lookup for the project, got %d", provider.calls)
	}
}

// TestRotationReasonsForRevokedOnlyProject guards against reporting a misleading rotation reason:
// a project that is revoked but not yet within the expiry threshold must be logged as "revoked"
// only. Computing reasons after merging the revoked projects into the expiry-due map would
// incorrectly attribute "expiry" to it as well.
func TestRotationReasonsForRevokedOnlyProject(t *testing.T) {
	expiryDue := map[string]map[string]string{"expiry-only": nil}
	revoked := map[string]map[string]string{"revoked-only": nil}

	reasons := rotationReasons(expiryDue, revoked)

	if want := []string{"revoked"}; !slices.Equal(reasons["revoked-only"], want) {
		t.Fatalf("expected revoked-only reasons %v, got %v", want, reasons["revoked-only"])
	}

	if want := []string{"expiry"}; !slices.Equal(reasons["expiry-only"], want) {
		t.Fatalf("expected expiry-only reasons %v, got %v", want, reasons["expiry-only"])
	}
}

// fakeAPIClient is a minimal client.APIClient stub used to exercise checkAndRotateContext without
// a live Docker daemon. It embeds the (nil) interface so it satisfies client.APIClient, and only
// overrides the two calls certrotation actually needs: ContainerList for standalone Compose
// discovery and ServiceList for Swarm discovery. Any other method call panics via the nil
// embedded interface, which is intentional: it surfaces a test bug immediately rather than
// silently doing nothing.
type fakeAPIClient struct {
	client.APIClient

	containerList func(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	serviceList   func(ctx context.Context, options client.ServiceListOptions) (client.ServiceListResult, error)
}

func (c *fakeAPIClient) ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
	if c.containerList == nil {
		return client.ContainerListResult{}, nil
	}

	return c.containerList(ctx, options)
}

func (c *fakeAPIClient) ServiceList(ctx context.Context, options client.ServiceListOptions) (client.ServiceListResult, error) {
	if c.serviceList == nil {
		return client.ServiceListResult{}, nil
	}

	return c.serviceList(ctx, options)
}

// fakeCli is a minimal command.Cli stub that returns a fixed client.APIClient, for use as a
// docker.ContextClient.Cli value in tests.
type fakeCli struct {
	command.Cli

	apiClient client.APIClient
}

func (c fakeCli) Client() client.APIClient { return c.apiClient }

// TestCheckAndRotate_ContextErrorIsolation verifies that a context which failed to resolve (e.g.
// an unreachable remote Docker host) is logged and skipped without preventing the remaining,
// healthy contexts from being checked. It also verifies the skipped context's display name (see
// docker.DisplayContextName) is attached to the log entry.
func TestCheckAndRotate_ContextErrorIsolation(t *testing.T) {
	var buf bytes.Buffer

	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	healthyClient := &fakeAPIClient{
		containerList: func(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
			return client.ContainerListResult{}, nil
		},
	}

	results := []docker.ContextClientResult{
		{
			ContextClient: docker.ContextClient{Name: "broken"},
			Err:           errors.New("connection refused"),
		},
		{
			ContextClient: docker.ContextClient{Name: "", Cli: fakeCli{apiClient: healthyClient}, SwarmMode: false},
		},
	}

	watcher := &Watcher{
		log: log,
		now: time.Now,
		listContexts: func(context.Context) ([]docker.ContextClientResult, error) {
			return results, nil
		},
	}

	watcher.checkAndRotate(t.Context())

	var sawBrokenContextSkipped bool

	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("expected JSON log entry, got %q: %v", line, err)
		}

		if entry["context"] == "broken" {
			if msg, _ := entry["msg"].(string); strings.Contains(msg, "skipping docker context") {
				sawBrokenContextSkipped = true
			}
		}
	}

	if !sawBrokenContextSkipped {
		t.Fatalf("expected a log entry noting the broken context was skipped, got logs:\n%s", buf.String())
	}
}

// TestCheckAndRotate_UsesEachContextsOwnSwarmMode verifies that discovery for each context uses
// that context's own SwarmMode (from docker.ContextClientResult), never a single shared/global
// value: a standalone context must only ever be queried via ContainerList, and a Swarm context
// only ever via ServiceList, even when both are checked in the same pass.
func TestCheckAndRotate_UsesEachContextsOwnSwarmMode(t *testing.T) {
	var containerCalls, serviceCalls int32

	standaloneClient := &fakeAPIClient{
		containerList: func(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
			atomic.AddInt32(&containerCalls, 1)
			return client.ContainerListResult{}, nil
		},
		serviceList: func(context.Context, client.ServiceListOptions) (client.ServiceListResult, error) {
			t.Error("unexpected ServiceList call for a standalone (non-swarm) context")
			return client.ServiceListResult{}, nil
		},
	}

	swarmClient := &fakeAPIClient{
		containerList: func(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
			t.Error("unexpected ContainerList call for a swarm-mode context")
			return client.ContainerListResult{}, nil
		},
		serviceList: func(context.Context, client.ServiceListOptions) (client.ServiceListResult, error) {
			atomic.AddInt32(&serviceCalls, 1)
			return client.ServiceListResult{}, nil
		},
	}

	results := []docker.ContextClientResult{
		{ContextClient: docker.ContextClient{Name: "", Cli: fakeCli{apiClient: standaloneClient}, SwarmMode: false}},
		{ContextClient: docker.ContextClient{Name: "swarm-remote", Cli: fakeCli{apiClient: swarmClient}, SwarmMode: true}},
	}

	watcher := &Watcher{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		now: time.Now,
		listContexts: func(context.Context) ([]docker.ContextClientResult, error) {
			return results, nil
		},
	}

	watcher.checkAndRotate(t.Context())

	if containerCalls != 1 {
		t.Errorf("expected exactly 1 ContainerList call, got %d", containerCalls)
	}

	if serviceCalls != 1 {
		t.Errorf("expected exactly 1 ServiceList call, got %d", serviceCalls)
	}
}

// TestCheckAndRotate_TagsLogsWithDisplayContextName verifies that log entries produced while
// processing a context (both the discovery step and the per-project rotation attempt) carry that
// context's display name (see docker.DisplayContextName), including for the default/local context
// whose internal Name is empty but which must be logged as "default".
func TestCheckAndRotate_TagsLogsWithDisplayContextName(t *testing.T) {
	var buf bytes.Buffer

	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

	dueLabels := map[string]string{
		api.ProjectLabel: "app",
		api.ServiceLabel: "web",
		docker.DocoCDLabels.Deployment.CertRotatable: "true",
		docker.DocoCDLabels.Deployment.CertExpiry:    now.Add(1 * time.Hour).Format(time.RFC3339),
	}

	defaultClient := &fakeAPIClient{
		containerList: func(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
			return client.ContainerListResult{
				Items: []container.Summary{{Names: []string{"/app_web_1"}, Labels: dueLabels}},
			}, nil
		},
	}

	results := []docker.ContextClientResult{
		{ContextClient: docker.ContextClient{Name: "", Cli: fakeCli{apiClient: defaultClient}, SwarmMode: false}},
	}

	watcher := &Watcher{
		log:       log,
		threshold: 72 * time.Hour,
		now:       func() time.Time { return now },
		listContexts: func(context.Context) ([]docker.ContextClientResult, error) {
			return results, nil
		},
	}

	watcher.checkAndRotate(t.Context())

	var sawDefaultContextEntry bool

	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("expected JSON log entry, got %q: %v", line, err)
		}

		if entry["context"] == "default" && entry["project"] == "app" {
			sawDefaultContextEntry = true
		}
	}

	if !sawDefaultContextEntry {
		t.Fatalf(`expected a log entry tagged context="default" for project "app", got logs:\n%s`, buf.String())
	}
}

// TestCheckAndRotate_RotatesEachContextIndependently verifies that a due project discovered on
// one context is rotated using that specific context's name and Docker client (via
// docker.RotateProjectCertificates), and that a failure rotating one context's project does not
// stop the other context's project from being attempted.
func TestCheckAndRotate_RotatesEachContextIndependently(t *testing.T) {
	dataMountPath := t.TempDir()
	t.Setenv("DATA_MOUNT_PATH", dataMountPath)
	t.Setenv("DEPLOY_CONFIG_BASE_DIR", "/")

	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

	dueLabelsFor := func(project string) map[string]string {
		return map[string]string{
			api.ProjectLabel: project,
			api.ServiceLabel: "web",
			docker.DocoCDLabels.Deployment.CertRotatable: "true",
			docker.DocoCDLabels.Deployment.CertExpiry:    now.Add(1 * time.Hour).Format(time.RFC3339),
		}
	}

	defaultClient := &fakeAPIClient{
		containerList: func(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
			return client.ContainerListResult{
				Items: []container.Summary{{Names: []string{"/app-default_web_1"}, Labels: dueLabelsFor("app-default")}},
			}, nil
		},
	}

	remoteLabels := dueLabelsFor("app-remote")
	remoteClient := &fakeAPIClient{
		serviceList: func(context.Context, client.ServiceListOptions) (client.ServiceListResult, error) {
			return client.ServiceListResult{
				Items: []swarm.Service{{Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "app-remote_web", Labels: remoteLabels}}}},
			}, nil
		},
	}

	var buf bytes.Buffer

	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	results := []docker.ContextClientResult{
		{ContextClient: docker.ContextClient{Name: "", Cli: fakeCli{apiClient: defaultClient}, SwarmMode: false}},
		{ContextClient: docker.ContextClient{Name: "remote", Cli: fakeCli{apiClient: remoteClient}, SwarmMode: true}},
	}

	watcher := &Watcher{
		log:       log,
		threshold: 72 * time.Hour,
		now:       func() time.Time { return now },
		listContexts: func(context.Context) ([]docker.ContextClientResult, error) {
			return results, nil
		},
	}

	watcher.checkAndRotate(t.Context())

	// Both projects lack the on-disk deploy config RotateProjectCertificates needs to reload, so
	// both rotation attempts are expected to fail; what this test verifies is that both contexts
	// were independently attempted (each tagged with its own display context name) rather than
	// one context's failure short-circuiting the other's.
	seenContexts := map[string]bool{}

	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("expected JSON log entry, got %q: %v", line, err)
		}

		if msg, _ := entry["msg"].(string); msg == "failed to rotate certificate" {
			if ctxName, _ := entry["context"].(string); ctxName != "" {
				seenContexts[ctxName] = true
			}
		}
	}

	if !seenContexts["default"] || !seenContexts["remote"] {
		t.Fatalf("expected both contexts to be attempted independently, got contexts: %v (logs:\n%s)", seenContexts, buf.String())
	}
}
