package docker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
)

func TestComposeScheduledServiceRefFromLabels(t *testing.T) {
	t.Parallel()

	t.Run("parses required and optional labels", func(t *testing.T) {
		t.Parallel()

		ref, err := composeScheduledServiceRefFromLabels(map[string]string{
			api.ProjectLabel:     "project-a",
			api.ServiceLabel:     "backup",
			api.WorkingDirLabel:  "/repo/stack",
			api.ConfigFilesLabel: "/repo/stack/compose.yaml, /repo/stack/compose.override.yaml",
		})
		if err != nil {
			t.Fatalf("composeScheduledServiceRefFromLabels() unexpected error: %v", err)
		}

		if ref.Project != "project-a" || ref.Service != "backup" {
			t.Fatalf("unexpected ref: %+v", ref)
		}

		if ref.WorkingDir != "/repo/stack" {
			t.Fatalf("unexpected working dir: %q", ref.WorkingDir)
		}

		if len(ref.ConfigFiles) != 2 {
			t.Fatalf("expected 2 config files, got %d (%v)", len(ref.ConfigFiles), ref.ConfigFiles)
		}
	})

	t.Run("fails when project or service labels are missing", func(t *testing.T) {
		t.Parallel()

		_, err := composeScheduledServiceRefFromLabels(map[string]string{
			api.WorkingDirLabel: "/repo/stack",
		})
		if !errors.Is(err, ErrComposeScheduledMetadataUnavailable) {
			t.Fatalf("expected ErrComposeScheduledMetadataUnavailable, got %v", err)
		}
	})

	t.Run("parses external secrets JSON label", func(t *testing.T) {
		t.Parallel()

		encodedRefs := map[string]string{
			"DB_PASSWORD": "my-secret-store/db-password",
			"API_KEY":     "my-secret-store/api-key",
		}
		refsJSON, _ := json.Marshal(encodedRefs)

		ref, err := composeScheduledServiceRefFromLabels(map[string]string{
			api.ProjectLabel:                "project-a",
			api.ServiceLabel:                "backup",
			api.WorkingDirLabel:             "/repo/stack",
			api.ConfigFilesLabel:            "/repo/stack/compose.yaml",
			DocoCDJobLabels.JobExternalRefs: string(refsJSON),
		})
		if err != nil {
			t.Fatalf("composeScheduledServiceRefFromLabels() unexpected error: %v", err)
		}

		if len(ref.EncodedExternalSecrets) != 2 {
			t.Fatalf("expected 2 external secrets, got %d: %v", len(ref.EncodedExternalSecrets), ref.EncodedExternalSecrets)
		}

		if ref.EncodedExternalSecrets["DB_PASSWORD"] != "my-secret-store/db-password" {
			t.Fatalf("unexpected DB_PASSWORD ref: %q", ref.EncodedExternalSecrets["DB_PASSWORD"])
		}
	})

	t.Run("ignores malformed external secrets JSON label", func(t *testing.T) {
		t.Parallel()

		ref, err := composeScheduledServiceRefFromLabels(map[string]string{
			api.ProjectLabel:                "project-a",
			api.ServiceLabel:                "backup",
			api.WorkingDirLabel:             "/repo/stack",
			api.ConfigFilesLabel:            "/repo/stack/compose.yaml",
			DocoCDJobLabels.JobExternalRefs: "not-valid-json",
		})
		if err != nil {
			t.Fatalf("composeScheduledServiceRefFromLabels() unexpected error: %v", err)
		}

		if len(ref.EncodedExternalSecrets) != 0 {
			t.Fatalf("expected empty external secrets on bad JSON, got %v", ref.EncodedExternalSecrets)
		}
	})
}

func TestSplitCommaSeparatedLabelValues(t *testing.T) {
	t.Parallel()

	got := splitCommaSeparatedLabelValues("a,, b , ,c")
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("unexpected values: %v", got)
	}
}

func TestLoadComposeScheduledProject_RequiresComposeMetadata(t *testing.T) {
	t.Parallel()

	_, err := loadComposeScheduledProject(context.Background(), nil, composeScheduledServiceRef{
		Project: "project-a",
		Service: "backup",
	}, nil)
	if !errors.Is(err, ErrComposeScheduledMetadataUnavailable) {
		t.Fatalf("expected ErrComposeScheduledMetadataUnavailable, got %v", err)
	}
}

func TestValidateComposeScheduledServiceScale(t *testing.T) {
	t.Parallel()

	ref := composeScheduledServiceRef{Project: "project-a", Service: "backup"}

	t.Run("accepts one replica", func(t *testing.T) {
		t.Parallel()

		project := &types.Project{
			Services: types.Services{
				"backup": {Name: "backup"},
			},
		}

		if err := validateComposeScheduledServiceScale(project, ref); err != nil {
			t.Fatalf("validateComposeScheduledServiceScale() unexpected error: %v", err)
		}
	})

	t.Run("rejects multiple replicas", func(t *testing.T) {
		t.Parallel()

		scale := 2
		project := &types.Project{
			Services: types.Services{
				"backup": {Name: "backup", Scale: &scale},
			},
		}

		err := validateComposeScheduledServiceScale(project, ref)
		if !errors.Is(err, ErrComposeScheduledServiceReplicated) {
			t.Fatalf("expected ErrComposeScheduledServiceReplicated, got %v", err)
		}
	})
}
