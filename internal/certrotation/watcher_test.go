package certrotation

import (
	"testing"
	"time"

	"github.com/docker/compose/v5/pkg/api"

	"github.com/kimdre/doco-cd/internal/docker"
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
