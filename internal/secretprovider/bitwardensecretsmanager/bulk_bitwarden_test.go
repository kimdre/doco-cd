//go:build !nobitwarden

package bitwardensecretsmanager

import (
	"errors"
	"fmt"
	"maps"
	"testing"

	"github.com/bitwarden/sdk-go/v2"
)

type fakeSecrets struct {
	sdk.SecretsInterface

	identifiers sdk.SecretIdentifiersResponse
	secrets     map[string]sdk.SecretResponse
	listErr     error
	getErr      error

	listedOrg  string
	batchSizes []int
}

func (f *fakeSecrets) List(organizationID string) (*sdk.SecretIdentifiersResponse, error) {
	f.listedOrg = organizationID

	return &f.identifiers, f.listErr
}

//nolint:revive // The method name is defined by the Bitwarden SDK interface.
func (f *fakeSecrets) GetByIDS(ids []string) (*sdk.SecretsResponse, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}

	f.batchSizes = append(f.batchSizes, len(ids))

	out := &sdk.SecretsResponse{}
	for _, id := range ids {
		out.Data = append(out.Data, f.secrets[id])
	}

	return out, nil
}

type fakeClient struct {
	sdk.BitwardenClientInterface

	secrets *fakeSecrets
}

func (f *fakeClient) Secrets() sdk.SecretsInterface { return f.secrets }

func newFakeSecrets(entries map[string]struct {
	key, value string
	projects   []string
},
) *fakeSecrets {
	f := &fakeSecrets{secrets: make(map[string]sdk.SecretResponse)}

	for id, entry := range entries {
		f.identifiers.Data = append(f.identifiers.Data, sdk.SecretIdentifierResponse{ID: id, Key: entry.key, ProjectIDS: entry.projects})
		f.secrets[id] = sdk.SecretResponse{ID: id, Key: entry.key, Value: entry.value}
	}

	return f
}

func TestProvider_GetSecretsByPath(t *testing.T) {
	t.Parallel()

	fake := newFakeSecrets(map[string]struct {
		key, value string
		projects   []string
	}{
		"1": {key: "DB_PASSWORD", value: "one", projects: []string{"proj-a"}},
		"2": {key: "API_KEY", value: "two", projects: []string{"proj-a", "proj-b"}},
		"3": {key: "OTHER", value: "three", projects: []string{"proj-b"}},
		"4": {key: "NO_PROJECT", value: "four"},
	})
	provider := &Provider{Client: &fakeClient{secrets: fake}}

	t.Run("whole organization", func(t *testing.T) {
		got, err := provider.GetSecretsByPath(t.Context(), "org-1", false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		want := map[string]string{"DB_PASSWORD": "one", "API_KEY": "two", "OTHER": "three", "NO_PROJECT": "four"}
		if !maps.Equal(got, want) {
			t.Errorf("GetSecretsByPath() = %v, want %v", got, want)
		}

		if fake.listedOrg != "org-1" {
			t.Errorf("listed organization = %q", fake.listedOrg)
		}
	})

	t.Run("single project", func(t *testing.T) {
		got, err := provider.GetSecretsByPath(t.Context(), "org-1:proj-a", true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !maps.Equal(got, map[string]string{"DB_PASSWORD": "one", "API_KEY": "two"}) {
			t.Errorf("GetSecretsByPath() = %v", got)
		}
	})
}

func TestProvider_GetSecretsByPath_Batches(t *testing.T) {
	t.Parallel()

	entries := make(map[string]struct {
		key, value string
		projects   []string
	})
	for i := range 250 {
		entries[fmt.Sprintf("id-%d", i)] = struct {
			key, value string
			projects   []string
		}{key: fmt.Sprintf("KEY_%d", i), value: "v"}
	}

	fake := newFakeSecrets(entries)

	got, err := (&Provider{Client: &fakeClient{secrets: fake}}).GetSecretsByPath(t.Context(), "org-1", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(got) != 250 {
		t.Errorf("expected 250 secrets, got %d", len(got))
	}

	if len(fake.batchSizes) != 3 || fake.batchSizes[0] != getByIDsBatchSize || fake.batchSizes[2] != 50 {
		t.Errorf("unexpected batch sizes: %v", fake.batchSizes)
	}
}

func TestProvider_GetSecretsByPath_DuplicateKey(t *testing.T) {
	t.Parallel()

	fake := newFakeSecrets(map[string]struct {
		key, value string
		projects   []string
	}{
		"1": {key: "TOKEN", value: "secret-one"},
		"2": {key: "TOKEN", value: "secret-two"},
	})

	_, err := (&Provider{Client: &fakeClient{secrets: fake}}).GetSecretsByPath(t.Context(), "org-1", false)
	if !errors.Is(err, ErrDuplicateSecretKey) {
		t.Fatalf("expected ErrDuplicateSecretKey, got %v", err)
	}
}

func TestProvider_GetSecretsByPath_Errors(t *testing.T) {
	t.Parallel()

	if _, err := (&Provider{Client: &fakeClient{secrets: &fakeSecrets{}}}).GetSecretsByPath(t.Context(), "", false); !errors.Is(err, ErrInvalidSecretPath) {
		t.Errorf("expected ErrInvalidSecretPath, got %v", err)
	}

	listErr := errors.New("list failed")

	if _, err := (&Provider{Client: &fakeClient{secrets: &fakeSecrets{listErr: listErr}}}).GetSecretsByPath(t.Context(), "org-1", false); !errors.Is(err, listErr) {
		t.Errorf("expected list error, got %v", err)
	}

	getErr := errors.New("get failed")

	fake := newFakeSecrets(map[string]struct {
		key, value string
		projects   []string
	}{"1": {key: "A", value: "v"}})
	fake.getErr = getErr

	if _, err := (&Provider{Client: &fakeClient{secrets: fake}}).GetSecretsByPath(t.Context(), "org-1", false); !errors.Is(err, getErr) {
		t.Errorf("expected get error, got %v", err)
	}
}
