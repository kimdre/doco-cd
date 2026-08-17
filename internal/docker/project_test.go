package docker

import (
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func TestProjectHash_ChangesWhenJobLabelsChange(t *testing.T) {
	t.Parallel()

	base := &types.Project{
		Services: types.Services{
			"worker": {
				Name: "worker",
				Labels: types.Labels{
					DocoCDJobLabels.JobEnabled:  "true",
					DocoCDJobLabels.JobSchedule: "@every 30m",
				},
			},
		},
	}

	changed := &types.Project{
		Services: types.Services{
			"worker": {
				Name: "worker",
				Labels: types.Labels{
					DocoCDJobLabels.JobEnabled:  "true",
					DocoCDJobLabels.JobSchedule: "@every 30s",
				},
			},
		},
	}

	baseHash, err := ProjectHash(base)
	if err != nil {
		t.Fatalf("ProjectHash(base) error: %v", err)
	}

	changedHash, err := ProjectHash(changed)
	if err != nil {
		t.Fatalf("ProjectHash(changed) error: %v", err)
	}

	if baseHash == changedHash {
		t.Fatalf("expected hash change for job schedule label update, got %q", baseHash)
	}
}

func TestProjectHash_ChangesWhenRecreateLabelsChange(t *testing.T) {
	t.Parallel()

	base := &types.Project{
		Services: types.Services{
			"api": {
				Name: "api",
				Labels: types.Labels{
					DocoCDLabels.Deployment.RecreateIgnore: "{configs: [nginx]}",
				},
			},
		},
	}

	changed := &types.Project{
		Services: types.Services{
			"api": {
				Name: "api",
				Labels: types.Labels{
					DocoCDLabels.Deployment.RecreateIgnore: "{configs: [app]}",
				},
			},
		},
	}

	baseHash, err := ProjectHash(base)
	if err != nil {
		t.Fatalf("ProjectHash(base) error: %v", err)
	}

	changedHash, err := ProjectHash(changed)
	if err != nil {
		t.Fatalf("ProjectHash(changed) error: %v", err)
	}

	if baseHash == changedHash {
		t.Fatalf("expected hash change for recreate label update, got %q", baseHash)
	}
}

func TestProjectHash_IgnoresDocoMetadataLabels(t *testing.T) {
	t.Parallel()

	base := &types.Project{
		Services: types.Services{
			"api": {
				Name: "api",
				Labels: types.Labels{
					DocoCDJobLabels.JobEnabled: "true",
				},
			},
		},
	}

	withMetadataChange := &types.Project{
		Services: types.Services{
			"api": {
				Name: "api",
				Labels: types.Labels{
					DocoCDJobLabels.JobEnabled:    "true",
					DocoCDLabels.Metadata.Version: "v-next",
				},
			},
		},
	}

	baseHash, err := ProjectHash(base)
	if err != nil {
		t.Fatalf("ProjectHash(base) error: %v", err)
	}

	withMetadataChangeHash, err := ProjectHash(withMetadataChange)
	if err != nil {
		t.Fatalf("ProjectHash(withMetadataChange) error: %v", err)
	}

	if baseHash != withMetadataChangeHash {
		t.Fatalf("expected metadata label changes to be ignored, got %q and %q", baseHash, withMetadataChangeHash)
	}
}

func TestProjectHash_IgnoresComposeGeneratedLabels(t *testing.T) {
	t.Parallel()

	base := &types.Project{
		Services: types.Services{
			"api": {
				Name: "api",
				Labels: types.Labels{
					DocoCDJobLabels.JobEnabled: "true",
				},
			},
		},
	}

	withComposeRuntimeLabel := &types.Project{
		Services: types.Services{
			"api": {
				Name: "api",
				Labels: types.Labels{
					DocoCDJobLabels.JobEnabled:   "true",
					"com.docker.compose.project": "another",
				},
			},
		},
	}

	baseHash, err := ProjectHash(base)
	if err != nil {
		t.Fatalf("ProjectHash(base) error: %v", err)
	}

	withComposeRuntimeLabelHash, err := ProjectHash(withComposeRuntimeLabel)
	if err != nil {
		t.Fatalf("ProjectHash(withComposeRuntimeLabel) error: %v", err)
	}

	if baseHash != withComposeRuntimeLabelHash {
		t.Fatalf("expected compose-generated label to be ignored, got %q and %q", baseHash, withComposeRuntimeLabelHash)
	}
}

func TestProjectHash_IgnoresScaleZeroServices(t *testing.T) {
	t.Parallel()

	base := &types.Project{
		Services: types.Services{
			"api": {
				Name:  "api",
				Image: "nginx:latest",
			},
		},
	}

	withScaledToZeroService := &types.Project{
		Services: types.Services{
			"api": {
				Name:  "api",
				Image: "nginx:latest",
			},
			"extra-tool": {
				Name:  "extra-tool",
				Image: "busybox:latest",
				Scale: new(0),
			},
		},
	}

	baseHash, err := ProjectHash(base)
	if err != nil {
		t.Fatalf("ProjectHash(base) error: %v", err)
	}

	withScaledToZeroServiceHash, err := ProjectHash(withScaledToZeroService)
	if err != nil {
		t.Fatalf("ProjectHash(withScaledToZeroService) error: %v", err)
	}

	if baseHash != withScaledToZeroServiceHash {
		t.Fatalf("expected scaled to zero services to be ignored, got %q and %q", baseHash, withScaledToZeroServiceHash)
	}
}

func TestWithNormalizedEnvValues_ReplacesServiceEnvValue(t *testing.T) {
	t.Parallel()

	certPEM := "-----BEGIN CERTIFICATE-----\nMIIFake...\n-----END CERTIFICATE-----\n"         // #nosec G101
	keyPEM := "-----BEGIN EC PRIVATE KEY-----\nMIIFakeKey...\n-----END EC PRIVATE KEY-----\n" // #nosec G101
	refString := "pki-role:pki:my-role:app.example.com"

	p := &types.Project{
		Services: types.Services{
			"app": {
				Name: "app",
				Environment: types.MappingWithEquals{
					"CERT":     new(certPEM),
					"CERT_KEY": new(keyPEM),
					"OTHER":    new("unchanged"),
				},
			},
		},
	}

	normMap := map[string]string{
		certPEM: refString,
		keyPEM:  refString + "_KEY",
	}

	got := WithNormalizedEnvValues(p, normMap)

	svc := got.Services["app"]

	if *svc.Environment["CERT"] != refString {
		t.Errorf("CERT: want %q, got %q", refString, *svc.Environment["CERT"])
	}

	if *svc.Environment["CERT_KEY"] != refString+"_KEY" {
		t.Errorf("CERT_KEY: want %q, got %q", refString+"_KEY", *svc.Environment["CERT_KEY"])
	}

	if *svc.Environment["OTHER"] != "unchanged" {
		t.Errorf("OTHER: want %q, got %q", "unchanged", *svc.Environment["OTHER"])
	}

	// Original project must not be modified.
	if *p.Services["app"].Environment["CERT"] != certPEM {
		t.Errorf("original project modified: CERT should still be cert PEM")
	}
}

func TestWithNormalizedEnvValues_ReplacesConfigContent(t *testing.T) {
	t.Parallel()

	certPEM := "-----BEGIN CERTIFICATE-----\nMIIFake...\n-----END CERTIFICATE-----\n"
	refString := "pki-role:pki:my-role:app.example.com"

	p := &types.Project{
		Configs: map[string]types.ConfigObjConfig{
			"app-cert": {Content: certPEM},
			"static":   {Content: "static content"},
		},
	}

	got := WithNormalizedEnvValues(p, map[string]string{certPEM: refString})

	if got.Configs["app-cert"].Content != refString {
		t.Errorf("config content: want %q, got %q", refString, got.Configs["app-cert"].Content)
	}

	if got.Configs["static"].Content != "static content" {
		t.Errorf("static config modified unexpectedly: %q", got.Configs["static"].Content)
	}
}

func TestWithNormalizedEnvValues_EmptyNormMap_ReturnsOriginal(t *testing.T) {
	t.Parallel()

	p := &types.Project{Services: types.Services{"a": {Name: "a"}}}

	if got := WithNormalizedEnvValues(p, nil); got != p {
		t.Error("expected original project pointer for nil normMap")
	}

	if got := WithNormalizedEnvValues(p, map[string]string{}); got != p {
		t.Error("expected original project pointer for empty normMap")
	}
}

// TestWithNormalizedEnvValues_StabilizesProjectHash verifies that two projects
// that differ only in the cert PEM value (simulating consecutive resolutions
// of the same pki-role ref) produce the same hash after normalization.
func TestWithNormalizedEnvValues_StabilizesProjectHash(t *testing.T) {
	t.Parallel()

	ref := "pki-role:pki:my-role:app.example.com"
	cert1 := "-----BEGIN CERTIFICATE-----\ncert-serial-1\n-----END CERTIFICATE-----\n"
	cert2 := "-----BEGIN CERTIFICATE-----\ncert-serial-2\n-----END CERTIFICATE-----\n"

	makeProject := func(certPEM string) *types.Project {
		return &types.Project{
			Services: types.Services{
				"app": {
					Name:        "app",
					Image:       "myapp:latest",
					Environment: types.MappingWithEquals{"CERT": new(certPEM)},
				},
			},
		}
	}

	norm1 := WithNormalizedEnvValues(makeProject(cert1), map[string]string{cert1: ref})
	norm2 := WithNormalizedEnvValues(makeProject(cert2), map[string]string{cert2: ref})

	h1, err := ProjectHash(norm1)
	if err != nil {
		t.Fatalf("ProjectHash error: %v", err)
	}

	h2, err := ProjectHash(norm2)
	if err != nil {
		t.Fatalf("ProjectHash error: %v", err)
	}

	if h1 != h2 {
		t.Errorf("hashes differ after normalization (cert re-issue caused spurious hash change): %q vs %q", h1, h2)
	}

	// Sanity check: without normalization the hashes DO differ.
	rawH1, _ := ProjectHash(makeProject(cert1))
	rawH2, _ := ProjectHash(makeProject(cert2))

	if rawH1 == rawH2 {
		t.Error("expected raw hashes to differ when cert PEM changes (test setup incorrect)")
	}
}
