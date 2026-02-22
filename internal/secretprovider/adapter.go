package secretprovider

import (
	"context"
	"io"
	"sync"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

// SecretProviderAdapter is a SecretProvider which breaks bulk operations down
// into individual tasks which in turn are passed to the internal SecretValueProvider
// instance for the actual data retrieval.
type SecretProviderAdapter struct {
	name string
	impl SecretValueProvider
}

// AdaptSecretValueProvider returns a SecretProviderAdapter using the provided
// SecretValueProvider instance for data retrieval.
func AdaptSecretValueProvider(name string, impl SecretValueProvider) *SecretProviderAdapter {
	result := &SecretProviderAdapter{
		name: name,
		impl: impl,
	}

	return result
}

// Name returns the value provided during construction.
func (p *SecretProviderAdapter) Name() string {
	return p.name
}

// Close forwards the call to the internal value provider if it implements
// a close logic; either by implementing [io.Closer] or simply a vanilla
// Close function.
func (p *SecretProviderAdapter) Close() {
	if c, ok := p.impl.(io.Closer); ok {
		_ = c.Close()
		return
	}

	type closer interface {
		Close()
	}

	if c, ok := p.impl.(closer); ok {
		c.Close()
		return
	}
}

// GetSecret forwards the call to the internal SecretValueProvider.
func (p *SecretProviderAdapter) GetSecret(ctx context.Context, id string) (string, error) {
	return p.impl.GetSecret(ctx, id)
}

// GetSecrets retrieves multiple secrets from Secrets Manager using the provided list of secret references.
func (p *SecretProviderAdapter) GetSecrets(ctx context.Context, refs []string) (map[string]string, error) {
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
func (p *SecretProviderAdapter) ResolveSecretReferences(ctx context.Context, secrets map[string]string) (secrettypes.ResolvedSecrets, error) {
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
