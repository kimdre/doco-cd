package docker

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

func contentDigestTestProject() *types.Project {
	return &types.Project{
		Environment: types.Mapping{
			"DB_PASSWORD": "secret-1",
		},
		Secrets: types.Secrets{
			"db_password": {Name: "db_password", Environment: "DB_PASSWORD"},
			"api_token":   {Name: "api_token", Content: "token-1"},
			"tls_key":     {Name: "tls_key", File: "/repo/tls.key"},
			"external":    {Name: "external", External: true},
		},
		Configs: types.Configs{
			"app_config": {Name: "app_config", Content: "debug: false"},
			"file_cfg":   {Name: "file_cfg", File: "/repo/app.yaml"},
		},
		Services: types.Services{
			"api": {
				Name: "api",
				Secrets: []types.ServiceSecretConfig{
					{Source: "db_password"},
					{Source: "api_token"},
				},
				Configs: []types.ServiceConfigObjConfig{
					{Source: "app_config"},
				},
			},
			"worker": {
				Name: "worker",
				Secrets: []types.ServiceSecretConfig{
					{Source: "tls_key"},
					{Source: "external"},
				},
				Configs: []types.ServiceConfigObjConfig{
					{Source: "file_cfg"},
				},
			},
			"cache": {
				Name: "cache",
			},
		},
	}
}

func contentDigestLabel(t *testing.T, p *types.Project, service string) (string, bool) {
	t.Helper()

	svc, ok := p.Services[service]
	if !ok {
		t.Fatalf("service %q not found", service)
	}

	v, ok := svc.Labels[DocoCDLabels.Deployment.ContentDigest]

	return v, ok
}

func applyContentDigest(t *testing.T, p *types.Project, refs map[string]secrettypes.ExternalSecretRef) {
	t.Helper()

	if err := ApplyContentDigestLabels(p, refs); err != nil {
		t.Fatalf("ApplyContentDigestLabels() error = %v", err)
	}
}

func TestApplyContentDigestLabels_OnlyContentConsumers(t *testing.T) {
	t.Parallel()

	p := contentDigestTestProject()
	applyContentDigest(t, p, nil)

	digest, _ := contentDigestLabel(t, p, "api")
	if decoded, err := hex.DecodeString(digest); err != nil || len(decoded) != 32 {
		t.Fatal("expected a SHA-256 hex digest on api")
	}

	for _, svc := range []string{"worker", "cache"} {
		if v, ok := contentDigestLabel(t, p, svc); ok {
			t.Fatalf("expected no content digest label on %s, got %q", svc, v)
		}
	}
}

func TestApplyContentDigestLabels_ValueChangesDigest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		change func(p *types.Project)
	}{
		{
			name: "environment secret",
			change: func(p *types.Project) {
				p.Environment["DB_PASSWORD"] = "secret-2"
			},
		},
		{
			name: "content secret",
			change: func(p *types.Project) {
				s := p.Secrets["api_token"]
				s.Content = "token-2"
				p.Secrets["api_token"] = s
			},
		},
		{
			name: "content config",
			change: func(p *types.Project) {
				c := p.Configs["app_config"]
				c.Content = "debug: true"
				p.Configs["app_config"] = c
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			base := contentDigestTestProject()
			applyContentDigest(t, base, nil)

			changed := contentDigestTestProject()
			tc.change(changed)
			applyContentDigest(t, changed, nil)

			baseDigest, _ := contentDigestLabel(t, base, "api")
			changedDigest, _ := contentDigestLabel(t, changed, "api")

			if baseDigest == changedDigest {
				t.Fatalf("expected digest change, got %q for both", baseDigest)
			}

			baseProjectHash, err := ProjectHash(base, "")
			if err != nil {
				t.Fatalf("ProjectHash(base) error = %v", err)
			}

			changedProjectHash, err := ProjectHash(changed, "")
			if err != nil {
				t.Fatalf("ProjectHash(changed) error = %v", err)
			}

			if baseProjectHash == changedProjectHash {
				t.Fatal("expected project hash change")
			}

			baseServiceHash, err := composeServiceHash(base.Services["api"])
			if err != nil {
				t.Fatalf("composeServiceHash(base) error = %v", err)
			}

			changedServiceHash, err := composeServiceHash(changed.Services["api"])
			if err != nil {
				t.Fatalf("composeServiceHash(changed) error = %v", err)
			}

			if baseServiceHash == changedServiceHash {
				t.Fatal("expected compose service config hash change")
			}
		})
	}
}

func TestApplyContentDigestLabels_StableForSameValues(t *testing.T) {
	t.Parallel()

	a := contentDigestTestProject()
	applyContentDigest(t, a, nil)

	b := contentDigestTestProject()
	applyContentDigest(t, b, nil)
	// A second call must not change the result.
	applyContentDigest(t, b, nil)

	digestA, _ := contentDigestLabel(t, a, "api")
	digestB, _ := contentDigestLabel(t, b, "api")

	if digestA != digestB {
		t.Fatalf("expected stable digest, got %q and %q", digestA, digestB)
	}
}

func TestApplyContentDigestLabels_MissingEnvironmentValue(t *testing.T) {
	t.Parallel()

	project := contentDigestTestProject()
	project.Environment["DB_PASSWORD"] = ""
	applyContentDigest(t, project, nil)

	emptyDigest, _ := contentDigestLabel(t, project, "api")

	delete(project.Environment, "DB_PASSWORD")
	applyContentDigest(t, project, nil)
	missingDigest, _ := contentDigestLabel(t, project, "api")

	if missingDigest == emptyDigest {
		t.Fatal("an unset environment variable must not hash like an empty value")
	}
}

func TestApplyContentDigestLabels_FileBasedChangeDoesNotChangeDigest(t *testing.T) {
	t.Parallel()

	base := contentDigestTestProject()
	applyContentDigest(t, base, nil)

	changed := contentDigestTestProject()
	s := changed.Secrets["tls_key"]
	s.File = "/repo/other.key"
	changed.Secrets["tls_key"] = s
	applyContentDigest(t, changed, nil)

	baseDigest, _ := contentDigestLabel(t, base, "api")
	changedDigest, _ := contentDigestLabel(t, changed, "api")

	if baseDigest != changedDigest {
		t.Fatal("expected file-based object change not to change the digest")
	}
}

func TestApplyContentDigestLabels_UsesPKISourceWhenLeafAndFullChainMatch(t *testing.T) {
	t.Parallel()

	const role = "pki-role:pki/issue/web"

	refs := map[string]secrettypes.ExternalSecretRef{"DB_PASSWORD": {LegacyRef: role}}

	first := contentDigestTestProject()
	first.Environment["DB_PASSWORD"] = role
	first.Environment["DB_PASSWORD_FULL"] = role + "_FULL"
	applyContentDigest(t, first, refs)

	second := contentDigestTestProject()
	second.Environment["DB_PASSWORD"] = "-----BEGIN CERTIFICATE-----A"
	second.Environment["DB_PASSWORD_FULL"] = second.Environment["DB_PASSWORD"]
	applyContentDigest(t, second, refs)

	firstDigest, _ := contentDigestLabel(t, first, "api")
	secondDigest, _ := contentDigestLabel(t, second, "api")

	if firstDigest != secondDigest {
		t.Fatal("expected the leaf digest to stay the same before and after certificate issuance")
	}
}

func TestApplyContentDigestLabels_RecreateIgnore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		ignore      string
		wantChanged bool
		wantLabel   bool
	}{
		{name: "ignore changed secret", ignore: "{secrets: [db_password]}", wantChanged: false, wantLabel: true},
		{name: "ignore other secret", ignore: "{secrets: [api_token]}", wantChanged: true, wantLabel: true},
		{name: "ignore other scope", ignore: "{configs: [app_config]}", wantChanged: true, wantLabel: true},
		{name: "ignore all secrets and configs", ignore: "{secrets: [], configs: []}", wantChanged: false, wantLabel: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			build := func(password string) *types.Project {
				p := contentDigestTestProject()
				p.Environment["DB_PASSWORD"] = password
				svc := p.Services["api"]
				svc.Labels = types.Labels{DocoCDLabels.Deployment.RecreateIgnore: tc.ignore}
				p.Services["api"] = svc

				return p
			}

			base := build("secret-1")
			applyContentDigest(t, base, nil)

			changed := build("secret-2")
			applyContentDigest(t, changed, nil)

			baseDigest, ok := contentDigestLabel(t, base, "api")
			if ok != tc.wantLabel {
				t.Fatalf("label present = %v, want %v", ok, tc.wantLabel)
			}

			changedDigest, _ := contentDigestLabel(t, changed, "api")

			if got := baseDigest != changedDigest; got != tc.wantChanged {
				t.Fatalf("digest changed = %v, want %v", got, tc.wantChanged)
			}
		})
	}
}

func TestApplyContentDigestLabels_InvalidIgnoreConfig(t *testing.T) {
	t.Parallel()

	withContent := contentDigestTestProject()
	svc := withContent.Services["api"]
	svc.Labels = types.Labels{DocoCDLabels.Deployment.RecreateIgnore: "{unknown: []}"}
	withContent.Services["api"] = svc

	err := ApplyContentDigestLabels(withContent, nil)
	if !errors.Is(err, ErrIgnoreCfgInvalid) {
		t.Fatalf("expected ErrIgnoreCfgInvalid, got %v", err)
	}

	withoutContent := &types.Project{
		Services: types.Services{
			"api": {
				Name:   "api",
				Labels: types.Labels{DocoCDLabels.Deployment.RecreateIgnore: "{unknown: []}"},
			},
		},
	}

	if err := ApplyContentDigestLabels(withoutContent, nil); err != nil {
		t.Fatalf("expected no error without content objects, got %v", err)
	}
}

func TestApplyContentDigestLabels_RemovesStaleLabel(t *testing.T) {
	t.Parallel()

	p := contentDigestTestProject()
	svc := p.Services["cache"]
	svc.Labels = types.Labels{DocoCDLabels.Deployment.ContentDigest: "stale"}
	p.Services["cache"] = svc

	applyContentDigest(t, p, nil)

	if v, ok := contentDigestLabel(t, p, "cache"); ok {
		t.Fatalf("expected stale label to be removed, got %q", v)
	}
}

func TestApplyContentDigestLabels_DoesNotMutateSharedLabels(t *testing.T) {
	t.Parallel()

	shared := types.Labels{"app": "api"}

	p := contentDigestTestProject()
	svc := p.Services["api"]
	svc.Labels = shared
	p.Services["api"] = svc

	applyContentDigest(t, p, nil)

	if _, ok := shared[DocoCDLabels.Deployment.ContentDigest]; ok {
		t.Fatal("expected the original labels map to stay unchanged")
	}

	if _, ok := contentDigestLabel(t, p, "api"); !ok {
		t.Fatal("expected the project service to get the label")
	}
}

func TestApplyContentDigestLabels_NilProject(t *testing.T) {
	t.Parallel()

	if err := ApplyContentDigestLabels(nil, nil); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestShouldIgnoreLabelInProjectHash_KeepsContentDigest(t *testing.T) {
	t.Parallel()

	if shouldIgnoreLabelInProjectHash(DocoCDLabels.Deployment.ContentDigest) {
		t.Fatal("expected the content digest label to be part of the project hash")
	}

	if !shouldIgnoreLabelInProjectHash(DocoCDLabels.Deployment.ContentDigest + ".unrelated") {
		t.Fatal("expected only the exact content digest label to be kept")
	}
}

func TestApplyContentDigestLabels_LoadComposeEnvironmentSecret(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	composeFile := filepath.Join(dir, "compose.yaml")

	content := `services:
  app:
    image: alpine:latest
    secrets:
      - db_password
    configs:
      - app_config
  other:
    image: alpine:latest
secrets:
  db_password:
    environment: DB_PASSWORD
configs:
  app_config:
    environment: APP_CONFIG
`

	if err := os.WriteFile(composeFile, []byte(content), 0o600); err != nil {
		t.Fatalf("write compose file: %v", err)
	}

	load := func(password, config string) *types.Project {
		p, err := LoadCompose(t.Context(), nil, dir, dir, "content-digest", []string{composeFile}, nil, nil,
			map[string]string{"DB_PASSWORD": password, "APP_CONFIG": config}, ComposeLoadOptions{})
		if err != nil {
			t.Fatalf("LoadCompose() error = %v", err)
		}

		applyContentDigest(t, p, nil)

		return p
	}

	first := load("secret-1", "level: info")
	newSecret := load("secret-2", "level: info")
	newConfig := load("secret-1", "level: debug")

	firstDigest, ok := contentDigestLabel(t, first, "app")
	if !ok {
		t.Fatal("expected content digest label on app")
	}

	for name, changed := range map[string]*types.Project{"secret": newSecret, "config": newConfig} {
		changedDigest, _ := contentDigestLabel(t, changed, "app")
		if firstDigest == changedDigest {
			t.Errorf("expected changed environment %s to change the digest", name)
		}
	}

	if _, ok := contentDigestLabel(t, first, "other"); ok {
		t.Fatal("expected no content digest label on a service without secrets")
	}

	firstHash, err := ProjectHash(first, dir)
	if err != nil {
		t.Fatalf("ProjectHash(first) error = %v", err)
	}

	secondHash, err := ProjectHash(newSecret, dir)
	if err != nil {
		t.Fatalf("ProjectHash(second) error = %v", err)
	}

	if firstHash == secondHash {
		t.Fatal("expected changed environment secret to change the project hash")
	}
}
