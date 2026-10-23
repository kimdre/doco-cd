package infisical

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	infisical "github.com/infisical/go-sdk"
	"github.com/infisical/go-sdk/packages/models"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

const (
	Name = "infisical"
)

var ErrInvalidSecretReference = errors.New("invalid secret reference")

// ErrUnresolvedSecretReference indicates that the value returned by Infisical still contains an
// Infisical secret-reference expression (e.g. "${DB_HOST}" or "${dev.DB_HOST}").
// Infisical's backend only replaces a reference when the referenced secret exists; when it doesn't
// (wrong key, wrong path, or deleted), the literal placeholder is left untouched in the returned value
// instead of failing the request (missing read permissions fail the request outright and are
// already surfaced as an error). The SDK does not expose a separate "fully resolved" status for
// this case, so doco-cd must detect it itself instead of silently injecting the unresolved
// placeholder text into the deployment.
// See https://github.com/Infisical/infisical/blob/main/backend/src/services/secret-v2-bridge/secret-reference-fns.ts.
var ErrUnresolvedSecretReference = errors.New("secret value contains an unresolved Infisical secret reference")

// secretReferencePattern matches Infisical's secret-reference syntax,
// e.g. "${KEY}", "${dev.KEY}", "${prod.frontend.KEY}" or the cross-project "${@project-slug.prod.KEY}".
// The character class mirrors Infisical's own INTERPOLATION_PATTERN_STRING so that hyphenated
// environment/project slugs and cross-project references are detected too.
var secretReferencePattern = regexp.MustCompile(`\$\{[A-Za-z0-9\-_.@]+}`)

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
		AutoTokenRefresh: new(true),
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

	if secretReferencePattern.MatchString(secret.SecretValue) {
		return "", fmt.Errorf("%w: %s", ErrUnresolvedSecretReference, ref)
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

// GetSecretsByPath retrieves all secrets in a folder of an Infisical project environment.
// The path format is "projectId:env:[/some/path/]". Without a folder, the root folder is used.
// With recursive set, secrets of all sub folders are included and their names contain the
// relative folder path (for example "sub/KEY").
func (p *Provider) GetSecretsByPath(_ context.Context, path string, recursive bool) (map[string]string, error) {
	projectId, env, folder, err := parseSecretPath(path)
	if err != nil {
		return nil, err
	}

	result, err := p.Client.Secrets().ListSecrets(infisical.ListSecretsOptions{
		Environment:            env,
		ProjectID:              projectId,
		SecretPath:             folder,
		Recursive:              recursive,
		ExpandSecretReferences: true,
		IncludeImports:         true,
		SkipUniqueValidation:   true,
	})
	if err != nil {
		return nil, err
	}

	return secretsToMap(folder, recursive, result.Secrets)
}

// secretsToMap converts listed secrets to a map of relative name to value.
// Without recursion, every secret keeps only its key as name. With recursion, secrets in
// sub folders get the relative folder in front of the key. Secrets from outside the folder
// (for example from imports) keep only their key as name.
func secretsToMap(folder string, recursive bool, secrets []models.Secret) (map[string]string, error) {
	out := make(map[string]string, len(secrets))

	for _, secret := range secrets {
		if secretReferencePattern.MatchString(secret.SecretValue) {
			return nil, fmt.Errorf("%w: %s", ErrUnresolvedSecretReference, secret.SecretKey)
		}

		name := secret.SecretKey

		if sub, ok := relativeFolder(folder, secret.SecretPath); recursive && ok && sub != "" {
			name = sub + "/" + secret.SecretKey
		}

		if _, exists := out[name]; exists {
			continue
		}

		out[name] = secret.SecretValue
	}

	return out, nil
}

// relativeFolder returns the folder of a secret relative to the requested folder.
// It reports false when the secret folder is not inside the requested folder.
func relativeFolder(folder, secretPath string) (string, bool) {
	secretPath = strings.Trim(secretPath, "/")
	base := strings.Trim(folder, "/")

	if base == "" {
		return secretPath, true
	}

	if secretPath == base {
		return "", true
	}

	if rest, ok := strings.CutPrefix(secretPath, base+"/"); ok {
		return rest, true
	}

	return "", false
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

// parseSecretPath parses a folder reference with the format "projectId:env:[/some/path/]".
// The returned folder always starts with a slash and has no trailing slash, except for the root folder "/".
func parseSecretPath(path string) (projectId, env, folder string, err error) {
	parts := strings.SplitN(path, ":", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", fmt.Errorf("%w: %s", ErrInvalidSecretReference, "expected format 'projectId:env:[/some/path/]'")
	}

	folder = "/"

	if len(parts) == 3 {
		trimmed := strings.Trim(parts[2], "/")
		if strings.Contains(trimmed, ":") {
			return "", "", "", fmt.Errorf("%w: %s", ErrInvalidSecretReference, "folder must not contain ':'")
		}

		if trimmed != "" {
			folder = "/" + trimmed
		}
	}

	return parts[0], parts[1], folder, nil
}
