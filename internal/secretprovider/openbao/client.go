package openbao

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	openbao "github.com/openbao/openbao/api/v2"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

const (
	Name = "openbao"
)

var ErrInvalidSecretReference = errors.New("invalid secret reference")

type Provider struct {
	Client *openbao.Client
}

// Name returns the name of the secret provider.
func (p *Provider) Name() string {
	return Name
}

// NewProvider creates a new Provider instance for OpenBao and performs login using the provided address and access token.
func NewProvider(_ context.Context, address, token string) (*Provider, error) {
	config := openbao.DefaultConfig()

	config.Address = address

	client, err := openbao.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("unable to initialize OpenBao client: %w", err)
	}

	client.SetToken(token)

	provider := &Provider{Client: client}

	return provider, nil
}

// GetSecret retrieves a secret value from the Secrets Manager using the provided secret reference.
func (p *Provider) GetSecret(ctx context.Context, ref string) (string, error) {
	secretEngine, id, key, err := parseSecretReference(ref)
	if err != nil {
		return "", err
	}

	secret, err := p.Client.KVv2(secretEngine).Get(ctx, id)
	if err != nil {
		return "", fmt.Errorf("failed to retrieve secret with ID %s: %w", id, err)
	}

	value, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("%w: key %s not found in secret %s", ErrInvalidSecretReference, key, id)
	}

	strValue, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("value of key %s in secret %s is not a string", key, id)
	}

	return strValue, nil
}

// GetSecrets retrieves multiple secrets from Secrets Manager using the provided list of secret references.
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

// parseSecretReference parses the secret reference string into its components: secretEngine, id, and key.
func parseSecretReference(ref string) (secretEngine, id, key string, err error) {
	// The reference format is "secretEngine:id:key"
	refFormat := `^[^:]+:[^:]+:[^:]+$`

	// Check if reference is in the correct format
	if matched, _ := regexp.MatchString(refFormat, ref); !matched {
		return "", "", "", fmt.Errorf("%w: %s", ErrInvalidSecretReference, "expected format 'secretEngine:id:key'")
	}

	parts := strings.SplitN(ref, ":", 3)
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("%w: %s", ErrInvalidSecretReference, "expected format 'secretEngine:id:key'")
	}

	return parts[0], parts[1], parts[2], nil
}
