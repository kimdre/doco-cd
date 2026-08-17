package certrotation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/docker/compose/v5/pkg/api"

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

	t.Run("latest expiry wins when prior scoped rotation leaves a stale label", func(t *testing.T) {
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

		if len(due) != 0 {
			t.Fatalf("expected stale label not to retrigger rotation, got %v", due)
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
}

type revocationCheckingProvider struct {
	revoked map[string]bool
	errFor  map[string]error
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

	if want := []string{"expiry"}; !slicesEqual(got["project-a"], want) {
		t.Fatalf("expected project-a reasons %v, got %v", want, got["project-a"])
	}

	if want := []string{"expiry", "revoked"}; !slicesEqual(got["project-b"], want) {
		t.Fatalf("expected project-b reasons %v, got %v", want, got["project-b"])
	}

	if want := []string{"revoked"}; !slicesEqual(got["project-c"], want) {
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

func slicesEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}

	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}

	return true
}
