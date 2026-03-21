// Package bitwardenvault provides a SecretProvider for Bitwarden/Vaultwarden using the bw CLI tool.
package bitwardenvault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kimdre/doco-cd/internal/filesystem"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

const (
	Name = "bitwarden_vault"
)

var (
	ErrUnsupportedArch = errors.New("unsupported architecture for bw CLI")
	ErrUnsupportedOS   = errors.New("unsupported OS for bw CLI")
)

// Provider uses the bw CLI to interact with Bitwarden/Vaultwarden.
type Provider struct {
	cliPath      string
	cliSession   string
	cliSessionMu sync.Mutex
	cfg          *Config
}

// NewProvider creates a Provider using the given config.
func NewProvider(apiUrl, tokenUrl, clientID, clientSecret string) (*Provider, error) {
	cfg := &Config{
		ApiUrl:             apiUrl,
		OAuth2ClientID:     clientID,
		OAuth2ClientSecret: clientSecret,
		OAuth2TokenURL:     tokenUrl,
	}

	cliPath, err := ensureBwCli()
	if err != nil {
		return nil, fmt.Errorf("failed to ensure bw CLI: %w", err)
	}

	return &Provider{cliPath: cliPath, cfg: cfg}, nil
}

func (p *Provider) Name() string {
	return Name
}

// ensureBwCli checks for the bw CLI and downloads it if missing.
func ensureBwCli() (string, error) {
	const (
		binName = "bw"
		binDir  = "./bin"
	)

	cliPath, err := exec.LookPath(binName)
	if err == nil {
		return cliPath, nil
	}

	err = os.MkdirAll(binDir, filesystem.PermDir)
	if err != nil {
		return "", fmt.Errorf("failed to create bin directory: %w", err)
	}

	cliPath = filepath.Join(binDir, binName)
	if _, err := os.Stat(cliPath); err == nil {
		return cliPath, nil
	}

	r, err := getLatestRelease()
	if err != nil {
		return "", fmt.Errorf("failed to get latest Bitwarden CLI release: %w", err)
	}

	err = downloadCLI(r, binDir, binName)
	if err != nil {
		return "", fmt.Errorf("failed to download Bitwarden CLI: %w", err)
	}

	return cliPath, nil
}

// getSession logs in to Bitwarden and returns a session key.
func (p *Provider) getSession(ctx context.Context) (string, error) {
	p.cliSessionMu.Lock()
	defer p.cliSessionMu.Unlock()

	if p.cliSession != "" {
		return p.cliSession, nil
	}
	// Validate client ID and secret for safe characters
	if strings.ContainsAny(p.cfg.OAuth2ClientID, " \t\n\r\v\f;|&$><`\"'\\") || strings.ContainsAny(p.cfg.OAuth2ClientSecret, " \t\n\r\v\f;|&$><`\"'\\") {
		return "", errors.New("client ID or secret contains unsafe characters")
	}

	env := os.Environ()
	env = append(env, "BW_CLIENTID="+p.cfg.OAuth2ClientID)
	env = append(env, "BW_CLIENTSECRET="+p.cfg.OAuth2ClientSecret)

	if p.cfg.ApiUrl == "https://vault.bitwarden.com" || p.cfg.ApiUrl == "https://vault.bitwarden.eu" {
		cmd := exec.CommandContext(ctx, p.cliPath, "config", "server", p.cfg.ApiUrl)

		cmd.Env = env
		cmd.Stderr = os.Stdout
		if err := cmd.Run(); err != nil {
			errOut, _ := cmd.Output()
			return "", fmt.Errorf("bw config server failed: %w", errOut)
		}
	} else {
		// Set server endpoints if provided
		cmd := exec.CommandContext(ctx, p.cliPath, "config", "server", "--api", p.cfg.ApiUrl, "--identity", p.cfg.OAuth2TokenURL)

		cmd.Env = env
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("bw config server --api failed: %w", err)
		}
	}

	// #nosec G204 -- Arguments are validated and not user-controlled; no shell is invoked.
	cmd := exec.CommandContext(ctx, p.cliPath, "login", "--apikey")
	cmd.Env = env

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("bw login failed: %w", err)
	}

	p.cliSession = strings.TrimSpace(string(out))

	return p.cliSession, nil
}

// GetSecret retrieves a secret by ID using the bw CLI.
func (p *Provider) GetSecret(ctx context.Context, id string) (string, error) {
	session, err := p.getSession(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get session: %w", err)
	}
	// Validate id and session for safe characters
	if strings.ContainsAny(id, " \t\n\r\v\f;|&$><`\"'\\") || strings.ContainsAny(session, " \t\n\r\v\f;|&$><`\"'\\") {
		return "", errors.New("secret id or session contains unsafe characters")
	}
	// #nosec G204 -- Arguments are validated and not user-controlled; no shell is invoked.
	cmd := exec.CommandContext(ctx, p.cliPath, "get", "item", id, "--session", session)

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("bw get item failed: %w", err)
	}
	// Parse the output JSON to extract the password or private key
	item := struct {
		Type  int `json:"type"`
		Login *struct {
			Password string `json:"password"`
		} `json:"login"`
		SSHKey *struct {
			PrivateKey string `json:"privateKey"`
		} `json:"sshKey"`
	}{}

	err = json.Unmarshal(out, &item)
	if err != nil {
		return "", fmt.Errorf("failed to parse bw item JSON: %w", err)
	}

	if item.Type == 1 && item.Login != nil {
		return item.Login.Password, nil
	}

	if item.Type == 6 && item.SSHKey != nil {
		return item.SSHKey.PrivateKey, nil
	}

	return "", errors.New("unsupported item type or missing value")
}

// GetSecrets retrieves multiple secrets by ID.
func (p *Provider) GetSecrets(ctx context.Context, ids []string) (map[string]string, error) {
	result := make(map[string]string)

	for _, id := range ids {
		val, err := p.GetSecret(ctx, id)
		if err != nil {
			return nil, err
		}

		result[id] = val
	}

	return result, nil
}

// ResolveSecretReferences resolves a map of env var to secret ID.
func (p *Provider) ResolveSecretReferences(ctx context.Context, secrets map[string]string) (secrettypes.ResolvedSecrets, error) {
	ids := make([]string, 0, len(secrets))
	for _, id := range secrets {
		ids = append(ids, id)
	}

	resolved, err := p.GetSecrets(ctx, ids)
	if err != nil {
		return nil, err
	}

	for envVar, secretID := range secrets {
		if val, ok := resolved[secretID]; ok {
			secrets[envVar] = val
		}
	}

	return secrettypes.ResolvedSecrets(secrets), nil
}

func (p *Provider) Close() {}
