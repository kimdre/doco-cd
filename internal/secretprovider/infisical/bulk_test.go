package infisical

import (
	"errors"
	"maps"
	"strings"
	"testing"

	infisical "github.com/infisical/go-sdk"
	"github.com/infisical/go-sdk/packages/models"
)

type fakeSecrets struct {
	infisical.SecretsInterface

	result  infisical.ListSecretsResult
	err     error
	options infisical.ListSecretsOptions
}

func (f *fakeSecrets) ListSecrets(options infisical.ListSecretsOptions) (infisical.ListSecretsResult, error) {
	f.options = options

	return f.result, f.err
}

type fakeClient struct {
	infisical.InfisicalClientInterface

	secrets *fakeSecrets
}

func (f *fakeClient) Secrets() infisical.SecretsInterface { return f.secrets }

func TestParseSecretPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path       string
		wantProj   string
		wantEnv    string
		wantFolder string
		wantErr    bool
	}{
		{path: "proj:dev", wantProj: "proj", wantEnv: "dev", wantFolder: "/"},
		{path: "proj:dev:", wantProj: "proj", wantEnv: "dev", wantFolder: "/"},
		{path: "proj:dev:/", wantProj: "proj", wantEnv: "dev", wantFolder: "/"},
		{path: "proj:dev:/apps/web/", wantProj: "proj", wantEnv: "dev", wantFolder: "/apps/web"},
		{path: "proj:dev:apps", wantProj: "proj", wantEnv: "dev", wantFolder: "/apps"},
		{path: "proj", wantErr: true},
		{path: ":dev", wantErr: true},
		{path: "proj:", wantErr: true},
		{path: "", wantErr: true},
		{path: "proj:dev:a:b", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()

			proj, env, folder, err := parseSecretPath(tt.path)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidSecretReference) {
					t.Fatalf("expected ErrInvalidSecretReference, got %v", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if proj != tt.wantProj || env != tt.wantEnv || folder != tt.wantFolder {
				t.Errorf("got (%q, %q, %q), want (%q, %q, %q)", proj, env, folder, tt.wantProj, tt.wantEnv, tt.wantFolder)
			}
		})
	}
}

func TestSecretsToMap(t *testing.T) {
	t.Parallel()

	secrets := []models.Secret{
		{SecretKey: "ROOT", SecretValue: "1", SecretPath: "/apps"},
		{SecretKey: "KEY", SecretValue: "2", SecretPath: "/apps/sub"},
		{SecretKey: "DEEP", SecretValue: "3", SecretPath: "/apps/sub/deeper"}, //nolint:gosec // Test data, not real credentials.
		{SecretKey: "IMPORTED", SecretValue: "4", SecretPath: "/shared"},
		{SecretKey: "NOPATH", SecretValue: "5"},
	}

	got, err := secretsToMap("/apps", true, secrets)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]string{
		"ROOT":            "1",
		"sub/KEY":         "2",
		"sub/deeper/DEEP": "3",
		"IMPORTED":        "4",
		"NOPATH":          "5",
	}
	if !maps.Equal(got, want) {
		t.Errorf("secretsToMap() = %v, want %v", got, want)
	}
}

func TestSecretsToMap_RootFolder(t *testing.T) {
	t.Parallel()

	got, err := secretsToMap("/", true, []models.Secret{
		{SecretKey: "TOP", SecretValue: "1", SecretPath: "/"},
		{SecretKey: "KEY", SecretValue: "2", SecretPath: "/sub"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !maps.Equal(got, map[string]string{"TOP": "1", "sub/KEY": "2"}) {
		t.Errorf("secretsToMap() = %v", got)
	}
}

// TestSecretsToMap_NotRecursive checks that imported secrets keep their key without
// recursion, also when the import folder is below the requested folder.
func TestSecretsToMap_NotRecursive(t *testing.T) {
	t.Parallel()

	got, err := secretsToMap("/", false, []models.Secret{
		{SecretKey: "TOP", SecretValue: "1", SecretPath: "/"},
		{SecretKey: "IMPORTED", SecretValue: "2", SecretPath: "/shared"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !maps.Equal(got, map[string]string{"TOP": "1", "IMPORTED": "2"}) {
		t.Errorf("secretsToMap() = %v", got)
	}
}

func TestSecretsToMap_UnresolvedReference(t *testing.T) {
	t.Parallel()

	_, err := secretsToMap("/", false, []models.Secret{{SecretKey: "BROKEN", SecretValue: "prefix-${MISSING}-suffix"}})
	if !errors.Is(err, ErrUnresolvedSecretReference) {
		t.Fatalf("expected ErrUnresolvedSecretReference, got %v", err)
	}

	if strings.Contains(err.Error(), "prefix-") {
		t.Errorf("error must not contain the secret value: %v", err)
	}
}

func TestProvider_GetSecretsByPath(t *testing.T) {
	t.Parallel()

	fake := &fakeSecrets{result: infisical.ListSecretsResult{Secrets: []models.Secret{
		{SecretKey: "DB_PASSWORD", SecretValue: "one", SecretPath: "/apps"},
	}}}
	provider := &Provider{Client: &fakeClient{secrets: fake}}

	got, err := provider.GetSecretsByPath(t.Context(), "proj:prod:/apps/", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !maps.Equal(got, map[string]string{"DB_PASSWORD": "one"}) {
		t.Errorf("GetSecretsByPath() = %v", got)
	}

	opts := fake.options
	if opts.ProjectID != "proj" || opts.Environment != "prod" || opts.SecretPath != "/apps" || !opts.Recursive || !opts.ExpandSecretReferences {
		t.Errorf("unexpected list options: %+v", opts)
	}
}

func TestProvider_GetSecretsByPath_Errors(t *testing.T) {
	t.Parallel()

	if _, err := (&Provider{Client: &fakeClient{secrets: &fakeSecrets{}}}).GetSecretsByPath(t.Context(), "invalid", false); !errors.Is(err, ErrInvalidSecretReference) {
		t.Errorf("expected ErrInvalidSecretReference, got %v", err)
	}

	listErr := errors.New("list failed")

	provider := &Provider{Client: &fakeClient{secrets: &fakeSecrets{err: listErr}}}
	if _, err := provider.GetSecretsByPath(t.Context(), "proj:prod", false); !errors.Is(err, listErr) {
		t.Errorf("expected list error, got %v", err)
	}
}
