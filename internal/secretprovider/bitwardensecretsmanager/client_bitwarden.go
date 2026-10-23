//go:build !nobitwarden

package bitwardensecretsmanager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/bitwarden/sdk-go/v2"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

const (
	Name          = "bitwarden_sm"
	stateFileName = "bitwarden-sm-state.json"
)

// stateFilePath returns the path to the Bitwarden state file, stored in a
// private (0700) per-user directory instead of a world-readable temp directory.
func stateFilePath() (string, error) {
	baseDir, err := os.UserConfigDir()
	if err != nil || baseDir == "" {
		baseDir = os.TempDir()
	}

	dir := filepath.Join(baseDir, "doco-cd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("failed to create state directory %s: %w", dir, err)
	}

	return filepath.Join(dir, stateFileName), nil
}

type Provider struct {
	Client sdk.BitwardenClientInterface
}

// Name returns the name of the secret provider.
func (p *Provider) Name() string {
	return Name
}

// NewProvider creates a new Provider instance for Bitwarden Secrets Manager and performs login using the provided access token.
func NewProvider(apiUrl, identityURL, accessToken string) (*Provider, error) {
	client, err := sdk.NewBitwardenClient(&apiUrl, &identityURL)
	if err != nil {
		return nil, err
	}

	provider := &Provider{Client: client}

	stateFile, err := stateFilePath()
	if err != nil {
		return nil, err
	}

	// Perform login with the provided access token
	err = provider.Client.AccessTokenLogin(accessToken, &stateFile)
	if err != nil {
		return nil, err
	}

	return provider, nil
}

// GetSecret retrieves a secret value from the Bitwarden Secrets Manager using the provided secret ID.
func (p *Provider) GetSecret(_ context.Context, id string) (string, error) {
	secret, err := p.Client.Secrets().Get(id)
	if err != nil {
		return "", err
	}

	return secret.Value, nil
}

// GetSecrets retrieves multiple secrets from Bitwarden Secrets Manager using the provided list of secret IDs.
func (p *Provider) GetSecrets(_ context.Context, ids []string) (map[string]string, error) {
	secrets, err := p.Client.Secrets().GetByIDS(ids)
	if err != nil {
		return nil, err
	}

	result := make(map[string]string)
	for _, secret := range secrets.Data {
		result[secret.ID] = secret.Value
	}

	return result, nil
}

// getByIDsBatchSize is the number of secrets requested per GetByIDS call.
const getByIDsBatchSize = 100

// GetSecretsByPath retrieves all secrets of an organization or of one project in it.
// The path format is "organizationId[:projectId]". Without a project ID, all secrets that the
// access token can read are returned. The returned names are the secret keys. Bitwarden has no
// folders, so the recursive argument has no effect.
func (p *Provider) GetSecretsByPath(_ context.Context, path string, _ bool) (map[string]string, error) {
	organizationID, projectID, err := parseSecretPath(path)
	if err != nil {
		return nil, err
	}

	identifiers, err := p.Client.Secrets().List(organizationID)
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(identifiers.Data))

	for _, identifier := range identifiers.Data {
		if projectID == "" || slices.Contains(identifier.ProjectIDS, projectID) {
			ids = append(ids, identifier.ID)
		}
	}

	out := make(map[string]string, len(ids))

	for batch := range slices.Chunk(ids, getByIDsBatchSize) {
		secrets, err := p.Client.Secrets().GetByIDS(batch)
		if err != nil {
			return nil, err
		}

		for _, secret := range secrets.Data {
			if _, exists := out[secret.Key]; exists {
				return nil, fmt.Errorf("%w: more than one secret has the key %q", ErrDuplicateSecretKey, secret.Key)
			}

			out[secret.Key] = secret.Value
		}
	}

	return out, nil
}

// ResolveSecretReferences resolves the provided map of environment variable names to secret IDs
// by fetching the corresponding secret values from the secret provider.
func (p *Provider) ResolveSecretReferences(ctx context.Context, secrets map[string]string) (secrettypes.ResolvedSecrets, error) {
	ids := make([]string, 0, len(secrets))
	for _, id := range secrets {
		ids = append(ids, id)
	}

	resolved, err := p.GetSecrets(ctx, ids)
	if err != nil {
		return nil, err
	}

	out := make(map[string]string, len(secrets))
	for envVar, secretID := range secrets {
		if val, ok := resolved[secretID]; ok {
			out[envVar] = val
		} else {
			out[envVar] = ""
		}
	}

	return out, nil
}

// Close cleans up resources used by the Provider.
func (p *Provider) Close() {
	p.Client.Close()
}
