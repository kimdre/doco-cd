// Package bitwardenvault provides a SecretProvider for Bitwarden/Vaultwarden using the bw CLI tool.
package bitwardenvault

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/kimdre/doco-cd/internal/filesystem"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

const (
	Name         = "bitwarden_vault"
	maxBwCliSize = 100 * 1024 * 1024 // 100MB max for CLI binary
)

// Provider uses the bw CLI to interact with Bitwarden/Vaultwarden.
type Provider struct {
	cliPath      string
	cliSession   string
	cliSessionMu sync.Mutex
	cfg          *Config
}

// NewProvider creates a Provider using the given config.
func NewProvider(apiUrl, tokenUrl, clientID, clientSecret string, skipTLSVerify bool) *Provider {
	cfg := &Config{
		ApiUrl:             apiUrl,
		OAuth2ClientID:     clientID,
		OAuth2ClientSecret: clientSecret,
		OAuth2TokenURL:     tokenUrl,
		SkipTLSVerify:      skipTLSVerify,
	}

	cliPath, err := ensureBwCli()
	if err != nil {
		panic(err)
	}

	return &Provider{cliPath: cliPath, cfg: cfg}
}

func (p *Provider) Name() string {
	return Name
}

// ensureBwCli checks for the bw CLI and downloads it if missing.
func ensureBwCli() (string, error) {
	if runtime.GOARCH != "amd64" {
		return "", fmt.Errorf("unsupported architecture for bw CLI: %s", runtime.GOARCH)
	}

	binName := "bw"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}

	cliPath, err := exec.LookPath(binName)
	if err == nil {
		return cliPath, nil
	}
	// Download bw CLI to ./bin/bw
	binDir := "./bin"

	err = os.MkdirAll(binDir, filesystem.PermDir)
	if err != nil {
		return "", fmt.Errorf("failed to create bin directory: %w", err)
	}

	cliPath = filepath.Join(binDir, binName)
	if _, err := os.Stat(cliPath); err == nil {
		return cliPath, nil
	}
	// Query GitHub API for latest release
	apiUrl := "https://api.github.com/repos/bitwarden/clients/releases/latest"
	resp, err := http.Get(apiUrl)
	if err != nil {
		return "", fmt.Errorf("failed to query Bitwarden GitHub releases: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}
	var release struct {
		Assets []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", fmt.Errorf("failed to decode GitHub release JSON: %w", err)
	}

	// Determine asset name pattern
	var assetPattern string
	switch runtime.GOOS {
	case "linux":
		if runtime.GOARCH == "amd64" {
			assetPattern = "bw-linux-"
		} else if runtime.GOARCH == "arm64" {
			assetPattern = "bw-linux-arm64-"
		} else {
			return "", fmt.Errorf("unsupported architecture for bw CLI: %s", runtime.GOARCH)
		}
	case "darwin":
		if runtime.GOARCH == "amd64" {
			assetPattern = "bw-macos-"
		} else if runtime.GOARCH == "arm64" {
			assetPattern = "bw-macos-arm64-"
		} else {
			return "", fmt.Errorf("unsupported architecture for bw CLI: %s", runtime.GOARCH)
		}
	case "windows":
		if runtime.GOARCH == "amd64" {
			assetPattern = "bw-windows-"
		} else if runtime.GOARCH == "arm64" {
			assetPattern = "bw-windows-arm64-"
		} else {
			return "", fmt.Errorf("unsupported architecture for bw CLI: %s", runtime.GOARCH)
		}
	default:
		return "", fmt.Errorf("unsupported OS for bw CLI: %s", runtime.GOOS)
	}

	var assetURL, assetName string
	for _, asset := range release.Assets {
		if strings.HasPrefix(asset.Name, assetPattern) && strings.HasSuffix(asset.Name, ".zip") {
			assetURL = asset.BrowserDownloadURL
			assetName = asset.Name
			break
		}
	}
	if assetURL == "" {
		return "", fmt.Errorf("could not find Bitwarden CLI asset for %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	zipPath := filepath.Join(binDir, assetName)
	out, err := os.Create(zipPath)
	if err != nil {
		return "", fmt.Errorf("failed to create zip file: %w", err)
	}
	defer func() {
		cerr := out.Close()
		if cerr != nil && err == nil {
			err = fmt.Errorf("failed to close zip file: %w", cerr)
		}
	}()
	resp2, err := http.Get(assetURL)
	if err != nil {
		return "", fmt.Errorf("failed to download bw CLI: %w", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to download bw CLI: status %d", resp2.StatusCode)
	}
	_, err = io.Copy(out, resp2.Body)
	if err != nil {
		return "", fmt.Errorf("failed to save bw CLI zip: %w", err)
	}
	// Unzip
	zipReader, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("failed to open bw CLI zip: %w", err)
	}
	defer func() {
		cerr := zipReader.Close()
		if cerr != nil && err == nil {
			err = fmt.Errorf("failed to close zip reader: %w", cerr)
		}
	}()
	found := false
	for _, f := range zipReader.File {
		// Only match the binary at the root (not in a subfolder)
		if filepath.Base(f.Name) == binName {
			binFile, ferr := os.OpenFile(cliPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, filesystem.PermDir)
			if ferr != nil {
				return "", fmt.Errorf("failed to create bw CLI binary: %w", ferr)
			}
			rc, ferr := f.Open()
			if ferr != nil {
				_ = binFile.Close()
				return "", fmt.Errorf("failed to open bw CLI file in zip: %w", ferr)
			}
			_, ferr = io.Copy(binFile, io.LimitReader(rc, maxBwCliSize))
			rcerr := rc.Close()
			bfcerr := binFile.Close()
			if ferr != nil {
				return "", fmt.Errorf("failed to extract bw CLI: %w", ferr)
			}
			if rcerr != nil {
				return "", fmt.Errorf("failed to close zip file reader: %w", rcerr)
			}
			if bfcerr != nil {
				return "", fmt.Errorf("failed to close bw CLI binary: %w", bfcerr)
			}
			// Set executable permissions
			if err := os.Chmod(cliPath, 0o755); err != nil {
				return "", fmt.Errorf("failed to set executable permission on bw CLI: %w", err)
			}
			found = true
			break
		}
	}

	if !found {
		return "", errors.New("bw CLI binary not found in zip archive")
	}

	err = os.Remove(zipPath)
	if err != nil {
		return "", fmt.Errorf("failed to remove zip file: %w", err)
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
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("bw config server failed: %w", err)
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
		return "", err
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
