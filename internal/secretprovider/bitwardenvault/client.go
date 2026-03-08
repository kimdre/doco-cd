// Package bitwardenvault provides a SecretProvider for Bitwarden/Vaultwarden REST API (vault items).
package bitwardenvault

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

type Provider struct {
	apiURL string
	client *http.Client
	// OAuth2 fields
	oauth2TokenURL     string
	oauth2ClientID     string
	oauth2ClientSecret string
	oauth2Token        string
	oauth2TokenExpiry  time.Time
	oauth2Mu           sync.Mutex
	skipTlsVerify      bool
}

const Name = "bitwarden_vault"

// NewProvider creates a Provider using the given config, including TLS options.
func NewProvider(apiUrl, tokenUrl, clientID, clientSecret string, skipTlsVerify bool) *Provider {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: skipTlsVerify},
	}

	return &Provider{
		apiURL:             apiUrl,
		client:             &http.Client{Transport: transport},
		oauth2TokenURL:     tokenUrl,
		oauth2ClientID:     clientID,
		oauth2ClientSecret: clientSecret,
		skipTlsVerify:      skipTlsVerify,
	}
}

func (p *Provider) Name() string {
	return "bitwarden_vault"
}

func (p *Provider) GetSecret(ctx context.Context, id string) (string, error) {
	item, err := p.getItemByID(ctx, id)
	if err != nil {
		return "", err
	}

	if item.Type == 1 && item.Login != nil {
		return item.Login.Password, nil
	}

	if item.Type == 6 && item.SSHKey != nil {
		return item.SSHKey.PrivateKey, nil
	}

	return "", errors.New("unsupported item type or missing value")
}

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

func (p *Provider) getAuthHeader(ctx context.Context) (string, error) {
	token, err := p.getOAuth2Token(ctx)
	if err != nil {
		return "", err
	}

	return "Bearer " + token, nil
}

func (p *Provider) getOAuth2Token(ctx context.Context) (string, error) {
	p.oauth2Mu.Lock()
	defer p.oauth2Mu.Unlock()

	if p.oauth2Token != "" && time.Now().Before(p.oauth2TokenExpiry.Add(-1*time.Minute)) {
		return p.oauth2Token, nil
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("scope", "api")
	form.Set("client_id", p.oauth2ClientID)
	form.Set("client_secret", p.oauth2ClientSecret)
	form.Set("device_identifier", "0") // Bitwarden/Vaultwarden requires this
	form.Set("deviceName", "docoCD")   // Bitwarden/Vaultwarden requires camelCase
	form.Set("deviceType", "0")        // Bitwarden/Vaultwarden requires camelCase
	form.Set("twoFactorToken", "0")

	req, err := http.NewRequestWithContext(ctx, "POST", p.oauth2TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("oauth2 token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	err = json.NewDecoder(resp.Body).Decode(&tokenResp)
	if err != nil {
		return "", err
	}

	p.oauth2Token = tokenResp.AccessToken
	p.oauth2TokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)

	return p.oauth2Token, nil
}

func (p *Provider) getItemByID(ctx context.Context, id string) (*VaultItem, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/object/item/%s", p.apiURL, id), nil)
	if err != nil {
		return nil, err
	}

	authHeader, err := p.getAuthHeader(ctx)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", authHeader)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		cerr := resp.Body.Close()
		if cerr != nil {
			// Optionally log or handle close error
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %s", resp.Status)
	}

	var item VaultItem
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		return nil, err
	}

	return &item, nil
}

type VaultItem struct {
	ID     string      `json:"id"`
	Type   int         `json:"type"`
	Login  *LoginData  `json:"login,omitempty"`
	SSHKey *SSHKeyData `json:"sshKey,omitempty"`
}

type LoginData struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type SSHKeyData struct {
	PrivateKey string `json:"privateKey"`
}
