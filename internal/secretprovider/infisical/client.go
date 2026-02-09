package infisical

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	infisical "github.com/infisical/go-sdk"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

const (
	Name = "infisical"
)

var ErrInvalidSecretReference = errors.New("invalid secret reference")

type Provider struct {
	Client infisical.InfisicalClientInterface
}

// Name returns the name of the secret provider.
func (p *Provider) Name() string {
	return Name
}

// NewProvider creates a new Provider instance for Infisical and performs login using the provided client ID and client secret.
func NewProvider(ctx context.Context, siteUrl, clientId, clientSecret string) (*Provider, error) {
	client := infisical.NewInfisicalClient(ctx, infisical.Config{
		SiteUrl:          siteUrl,
		AutoTokenRefresh: true,
	})

	provider := &Provider{Client: client}

	// Perform login with the provided access token
	_, err := client.Auth().UniversalAuthLogin(clientId, clientSecret)
	if err != nil {
		return nil, err
	}

	return provider, nil
}

// GetSecret retrieves a secret value from the Secrets Manager using the provided secret ID.
func (p *Provider) GetSecret(_ context.Context, ref string) (string, error) {
	projectId, env, key, path, err := parseSecretReference(ref)
	if err != nil {
		return "", err
	}

	secret, err := p.Client.Secrets().Retrieve(infisical.RetrieveSecretOptions{
		SecretKey:              key,
		Environment:            env,
		ProjectID:              projectId,
		SecretPath:             path,
		ExpandSecretReferences: true,
		IncludeImports:         true,
	})
	if err != nil {
		return "", err
	}

	return secret.SecretValue, nil
}

// GetSecrets retrieves multiple secrets from Secrets Manager using the provided list of secret IDs.
func (p *Provider) GetSecrets(ctx context.Context, refs []string) (map[string]string, error) {
	resolvedSecrets := make(map[string]string)

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 1)

	for _, ref := range refs {
		wg.Add(1)

		go func(secretName string) {
			defer wg.Done()

			v, err := p.GetSecret(ctx, ref)
			if err != nil {
				select {
				case errCh <- err:
					cancel()
				default:
				}

				return
			}

			mu.Lock()

			resolvedSecrets[secretName] = v

			mu.Unlock()
		}(ref)
	}

	wg.Wait()
	close(errCh)

	if err, ok := <-errCh; ok {
		return nil, err
	}

	return resolvedSecrets, nil
}

// ResolveSecretReferences resolves the provided map of environment variable names to secret IDs
// by fetching the corresponding secret values from the secret provider.
func (p *Provider) ResolveSecretReferences(ctx context.Context, secrets map[string]string) (secrettypes.ResolvedSecrets, error) {
	refs := make([]string, 0, len(secrets))
	for _, id := range secrets {
		refs = append(refs, id)
	}

	resolved, err := p.GetSecrets(ctx, refs)
	if err != nil {
		return nil, err
	}

	for envVar, secretID := range secrets {
		if val, ok := resolved[secretID]; ok {
			secrets[envVar] = val
		}
	}

	return secrets, nil
}

// Close cleans up resources used by the Provider.
func (p *Provider) Close() {}

// parseSecretReference parses the secret reference string into its components: env, projectId, key, and path.
func parseSecretReference(ref string) (projectId, env, key, path string, err error) {
	// The reference format is "projectId:env:[/some/path/]key"
	refFormat := `^[^:]+:[^:]+:(/?(?:[^/:]+/)*[^/:]+)$`

	// Check if reference is in the correct format
	if matched, _ := regexp.MatchString(refFormat, ref); !matched {
		return "", "", "", "", fmt.Errorf("%w: %s", ErrInvalidSecretReference, "expected format 'projectId:env:[/some/path/]key'")
	}

	parts := strings.SplitN(ref, ":", 3)
	if len(parts) != 3 {
		return "", "", "", "", fmt.Errorf("%w: %s", ErrInvalidSecretReference, "expected format 'projectId:env:[/some/path/]key'")
	}

	// Further split the third part into path and key
	pathAndKey := parts[2]

	lastSlashIndex := strings.LastIndex(pathAndKey, "/")
	if lastSlashIndex == -1 {
		// No path, only key
		return parts[0], parts[1], pathAndKey, "/", nil
	}

	key = pathAndKey[lastSlashIndex+1:]
	path = pathAndKey[:lastSlashIndex]

	if path != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	return parts[0], parts[1], key, path, nil
}
