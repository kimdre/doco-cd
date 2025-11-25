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

const (
	PKIRefFormat    = `^pki:(?:[^:]+:)?[^:]+:[^:]+$`      // #nosec G101 pki:<namespace(optional)>:<secretEngine>:<commonName>
	SecretRefFormat = `^kv:(?:[^:]+:)?[^:]+:[^:]+:[^:]+$` // #nosec G101 kv:<namespace(optional)>:<secretEngine>:<secretName>:<key>
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
	namespace, engineType, engineName, id, key, err := parseReference(ref)
	if err != nil {
		return "", err
	}

	c := p.Client.WithNamespace(namespace)

	var strValue string

	switch engineType {
	case "pki":
		serial, err := GetCertSerial(ctx, c, engineName, id)
		if err != nil {
			return "", fmt.Errorf("failed to retrieve certificate serial for common name %s: %w", id, err)
		}

		strValue, err = GetCert(ctx, c, engineName, serial)
		if err != nil {
			return "", fmt.Errorf("failed to retrieve certificate with serial %s: %w", id, err)
		}

	case "kv":
		strValue, err = GetSecret(ctx, c, engineName, id, key)
		if err != nil {
			return "", fmt.Errorf("failed to retrieve secret with id %s: %w", id, err)
		}
	default:
		return "", fmt.Errorf("%w: unknown secret engine type %s", ErrInvalidSecretReference, engineType)
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

// parseReference parses the reference string into its components: engineType, engineName, id, and key.
func parseReference(ref string) (namespace, engineType, engineName, id, key string, err error) {
	const defaultNamespace = "root"

	matchedPKI, _ := regexp.MatchString(PKIRefFormat, ref)
	matchedSecret, _ := regexp.MatchString(SecretRefFormat, ref)

	// Check if reference is in the correct format
	if !matchedPKI && !matchedSecret {
		return "", "", "", "", "", fmt.Errorf("%w: %s", ErrInvalidSecretReference, "unexpected ref format")
	}

	// Handle PKI reference
	if matchedPKI {
		parts := strings.Split(ref, ":")
		if len(parts) == 3 {
			// pki:<engineType>:<commonName>
			return defaultNamespace, parts[0], parts[1], parts[2], "", nil
		} else if len(parts) == 4 {
			// pki:<namespace>:<engineType>:<commonName>
			return parts[1], parts[0], parts[2], parts[3], "", nil
		}

		return "", "", "", "", "", fmt.Errorf("%w: %s", ErrInvalidSecretReference, "expected format 'pki:<namespace(optional)>:<secretEngine>:<commonName>'")
	}

	// Handle Secret reference
	parts := strings.Split(ref, ":")
	if len(parts) == 4 {
		// kv:<engineType>:<secretName>:<key>
		return defaultNamespace, parts[0], parts[1], parts[2], parts[3], nil
	} else if len(parts) == 5 {
		// kv:<namespace>:<engineType>:<secretName>:<key>
		return parts[1], parts[0], parts[2], parts[3], parts[4], nil
	}

	return "", "", "", "", "", fmt.Errorf("%w: %s", ErrInvalidSecretReference, "expected format 'kv:<namespace(optional)>:<secretEngine>:<secretName>:<key>'")
}
