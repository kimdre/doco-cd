package openbao

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/kimdre/doco-cd/internal/config"
)

var errEmptyAccessTokenFile = errors.New("SECRET_PROVIDER_ACCESS_TOKEN_FILE must contain a nonempty token")

// accessTokenFile reads the access token file again for each call, so rotated tokens take effect.
// It keeps the last valid token with the raw file content. Unchanged content skips SOPS
// decryption, which can call a remote KMS. Changed content never returns the old token.
type accessTokenFile struct {
	path string

	mu      sync.Mutex
	content []byte
	token   string
}

// newAccessTokenFile creates a new accessTokenFile for the given path.
func newAccessTokenFile(path string) *accessTokenFile {
	return &accessTokenFile{path: path}
}

// Token returns the token from the current file content.
func (f *accessTokenFile) Token() (string, error) {
	content, err := os.ReadFile(f.path) // #nosec G304 -- The path is configured by the operator.
	if err != nil {
		return "", fmt.Errorf("read SECRET_PROVIDER_ACCESS_TOKEN_FILE: %w", err)
	}

	// The lock also covers parsing, so concurrent operations decrypt new content only once.
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.token != "" && bytes.Equal(content, f.content) {
		return f.token, nil
	}

	token, err := parseAccessToken(content, f.path)
	if err != nil {
		return "", err
	}

	f.content = content
	f.token = token

	return token, nil
}

// parseAccessToken parses the access token from the given file content and path.
func parseAccessToken(content []byte, path string) (string, error) {
	fileValue := strings.TrimSpace(string(content))
	if fileValue == "" {
		return "", errEmptyAccessTokenFile
	}

	var token string

	mappings := []config.EnvVarFileMapping{
		{EnvName: "SECRET_PROVIDER_ACCESS_TOKEN", EnvValue: &token, FileValue: &fileValue, FilePath: path},
	}
	if err := config.LoadFileBasedEnvVars(&mappings); err != nil {
		return "", fmt.Errorf("load SECRET_PROVIDER_ACCESS_TOKEN_FILE: %w", err)
	}

	// SOPS content can decrypt to an empty value.
	if token == "" {
		return "", errEmptyAccessTokenFile
	}

	return token, nil
}
