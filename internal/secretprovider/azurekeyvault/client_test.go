package azurekeyvault

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
)

type fakeSecretClient struct {
	name     string
	version  string
	response azsecrets.GetSecretResponse
	err      error

	// listPages holds the pages returned by NewListSecretPropertiesPager.
	listPages []azsecrets.ListSecretPropertiesResponse
	listErr   error
	// values maps a secret name to its value for GetSecret calls.
	values map[string]string
}

func (f *fakeSecretClient) GetSecret(
	_ context.Context,
	name, version string,
	_ *azsecrets.GetSecretOptions,
) (azsecrets.GetSecretResponse, error) {
	f.name = name
	f.version = version

	if value, ok := f.values[name]; ok {
		return azsecrets.GetSecretResponse{Value: &value}, f.err
	}

	return f.response, f.err
}

func (f *fakeSecretClient) NewListSecretPropertiesPager(
	_ *azsecrets.ListSecretPropertiesOptions,
) *runtime.Pager[azsecrets.ListSecretPropertiesResponse] {
	index := 0

	return runtime.NewPager(runtime.PagingHandler[azsecrets.ListSecretPropertiesResponse]{
		More: func(_ azsecrets.ListSecretPropertiesResponse) bool {
			return index < len(f.listPages)
		},
		Fetcher: func(_ context.Context, _ *azsecrets.ListSecretPropertiesResponse) (azsecrets.ListSecretPropertiesResponse, error) {
			if f.listErr != nil {
				return azsecrets.ListSecretPropertiesResponse{}, f.listErr
			}

			page := f.listPages[index]
			index++

			return page, nil
		},
	})
}

func secretProperties(name string, enabled bool) *azsecrets.SecretProperties {
	id := azsecrets.ID("https://vault.example/secrets/" + name + "/0123456789abcdef")

	return &azsecrets.SecretProperties{
		ID:         &id,
		Attributes: &azsecrets.SecretAttributes{Enabled: &enabled},
	}
}

func listPage(properties ...*azsecrets.SecretProperties) azsecrets.ListSecretPropertiesResponse {
	return azsecrets.ListSecretPropertiesResponse{
		Value: properties,
	}
}

func TestValueProviderGetSecretsByPath(t *testing.T) {
	client := &fakeSecretClient{
		listPages: []azsecrets.ListSecretPropertiesResponse{
			listPage(secretProperties("app-db-password", true), secretProperties("other-token", true)),
			listPage(secretProperties("app-api-key", true), secretProperties("app-disabled", false), nil),
		},
		values: map[string]string{
			"app-db-password": "one",
			"app-api-key":     "two",
			"other-token":     "three",
			"app-disabled":    "four",
		},
	}

	got, err := newValueProvider(client).GetSecretsByPath(t.Context(), "app-", false)
	if err != nil {
		t.Fatalf("GetSecretsByPath() error = %v", err)
	}

	want := map[string]string{"db-password": "one", "api-key": "two"}
	if !maps.Equal(got, want) {
		t.Fatalf("GetSecretsByPath() = %v, want %v", got, want)
	}
}

func TestValueProviderGetSecretsByPathAll(t *testing.T) {
	client := &fakeSecretClient{
		listPages: []azsecrets.ListSecretPropertiesResponse{listPage(secretProperties("a", true), secretProperties("b", true))},
		values:    map[string]string{"a": "1", "b": "2"},
	}

	got, err := newValueProvider(client).GetSecretsByPath(t.Context(), "/", true)
	if err != nil {
		t.Fatalf("GetSecretsByPath() error = %v", err)
	}

	if !maps.Equal(got, map[string]string{"a": "1", "b": "2"}) {
		t.Fatalf("GetSecretsByPath() = %v", got)
	}
}

func TestValueProviderGetSecretsByPathIgnoresCase(t *testing.T) {
	client := &fakeSecretClient{
		listPages: []azsecrets.ListSecretPropertiesResponse{listPage(secretProperties("App-Token", true), secretProperties("app", true))},
		values:    map[string]string{"App-Token": "1", "app": "2"},
	}

	got, err := newValueProvider(client).GetSecretsByPath(t.Context(), "app-", false)
	if err != nil {
		t.Fatalf("GetSecretsByPath() error = %v", err)
	}

	if !maps.Equal(got, map[string]string{"Token": "1"}) {
		t.Fatalf("GetSecretsByPath() = %v", got)
	}
}

func TestValueProviderGetSecretsByPathPropagatesErrors(t *testing.T) {
	listErr := errors.New("list failed")

	if _, err := newValueProvider(&fakeSecretClient{
		listPages: []azsecrets.ListSecretPropertiesResponse{{}},
		listErr:   listErr,
	}).GetSecretsByPath(t.Context(), "app-", false); !errors.Is(err, listErr) {
		t.Fatalf("GetSecretsByPath() error = %v, want list error", err)
	}

	getErr := errors.New("get failed")

	if _, err := newValueProvider(&fakeSecretClient{
		listPages: []azsecrets.ListSecretPropertiesResponse{listPage(secretProperties("app-x", true))},
		err:       getErr,
	}).GetSecretsByPath(t.Context(), "app-", false); !errors.Is(err, getErr) {
		t.Fatalf("GetSecretsByPath() error = %v, want get error", err)
	}
}

func TestValueProviderGetSecret(t *testing.T) {
	value := "secret-value"

	for _, test := range []struct {
		name        string
		ref         string
		wantName    string
		wantVersion string
	}{
		{
			name:     "latest version",
			ref:      "database-password",
			wantName: "database-password",
		},
		{
			name:        "pinned version",
			ref:         "database-password/7c3cc89647ba4b9f8dd7d8d5f92d00b9",
			wantName:    "database-password",
			wantVersion: "7c3cc89647ba4b9f8dd7d8d5f92d00b9",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeSecretClient{
				response: azsecrets.GetSecretResponse{
					Secret: azsecrets.Secret{Value: &value},
				},
			}
			provider := newValueProvider(client)

			got, err := provider.GetSecret(t.Context(), test.ref)
			if err != nil {
				t.Fatalf("GetSecret() error = %v", err)
			}

			if got != value {
				t.Fatalf("GetSecret() = %q, want %q", got, value)
			}

			if client.name != test.wantName {
				t.Fatalf("Azure secret name = %q, want %q", client.name, test.wantName)
			}

			if client.version != test.wantVersion {
				t.Fatalf("Azure secret version = %q, want %q", client.version, test.wantVersion)
			}
		})
	}
}

func TestValueProviderGetSecretRejectsInvalidReference(t *testing.T) {
	for _, ref := range []string{"", "/version", "name/", "name/version/extra"} {
		t.Run(ref, func(t *testing.T) {
			provider := newValueProvider(&fakeSecretClient{})

			_, err := provider.GetSecret(t.Context(), ref)
			if !errors.Is(err, ErrInvalidSecretReference) {
				t.Fatalf("GetSecret() error = %v, want ErrInvalidSecretReference", err)
			}
		})
	}
}

func TestValueProviderGetSecretPropagatesClientError(t *testing.T) {
	clientErr := errors.New("azure request failed")
	provider := newValueProvider(&fakeSecretClient{err: clientErr})

	_, err := provider.GetSecret(t.Context(), "database-password")
	if !errors.Is(err, clientErr) {
		t.Fatalf("GetSecret() error = %v, want client error", err)
	}
}

func TestValueProviderGetSecretRejectsMissingValue(t *testing.T) {
	provider := newValueProvider(&fakeSecretClient{})

	_, err := provider.GetSecret(t.Context(), "database-password")
	if !errors.Is(err, ErrSecretValueMissing) {
		t.Fatalf("GetSecret() error = %v, want ErrSecretValueMissing", err)
	}
}

func TestNewCredentialUsesClientSecretCredentialForSecretFile(t *testing.T) {
	cfg := &Config{
		TenantID:             "11111111-1111-1111-1111-111111111111",
		ClientID:             "22222222-2222-2222-2222-222222222222",
		ClientSecret:         "client-secret",
		clientSecretFromFile: true,
	}

	credential, err := newCredential(cfg)
	if err != nil {
		t.Fatalf("newCredential() error = %v", err)
	}

	if _, ok := credential.(*azidentity.ClientSecretCredential); !ok {
		t.Fatalf("newCredential() type = %T, want *azidentity.ClientSecretCredential", credential)
	}
}

func TestNewCredentialRejectsIncompleteSecretFileCredentials(t *testing.T) {
	cfg := &Config{
		ClientSecret:         "client-secret",
		clientSecretFromFile: true,
	}

	if _, err := newCredential(cfg); err == nil {
		t.Fatal("newCredential() error = nil, want error")
	}
}
