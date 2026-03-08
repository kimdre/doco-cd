// Package bitwardenvault provides a SecretProvider for Bitwarden/Vaultwarden REST API (vault items).
package bitwardenvault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

type Provider struct {
	ApiUrl string
	ApiKey string // Used if OAuth2 is not configured
	client *http.Client

	// OAuth2 fields
	oauth2TokenURL     string
	oauth2ClientID     string
	oauth2ClientSecret string
	oauth2Token        string
	oauth2TokenExpiry  time.Time
	oauth2Mu           sync.Mutex
}

const Name = "bitwarden_vault"

func NewProvider(apiUrl, apiKey string, opts ...func(*Provider)) *Provider {
	p := &Provider{
		ApiUrl: apiUrl,
		ApiKey: apiKey,
		client: &http.Client{},
	}
	for _, opt := range opts {
		opt(p)
	}

	return p
}

// WithOAuth2 configures the provider to use OAuth2 client credentials.
func WithOAuth2(tokenURL, clientID, clientSecret string) func(*Provider) {
	return func(p *Provider) {
		p.oauth2TokenURL = tokenURL
		p.oauth2ClientID = clientID
		p.oauth2ClientSecret = clientSecret
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
	if p.oauth2TokenURL != "" && p.oauth2ClientID != "" && p.oauth2ClientSecret != "" {
		token, err := p.getOAuth2Token(ctx)
		if err != nil {
			return "", err
		}

		return "Bearer " + token, nil
	}

	return "Bearer " + p.ApiKey, nil
}

func (p *Provider) getOAuth2Token(ctx context.Context) (string, error) {
	p.oauth2Mu.Lock()
	defer p.oauth2Mu.Unlock()

	if p.oauth2Token != "" && time.Now().Before(p.oauth2TokenExpiry.Add(-1*time.Minute)) {
		return p.oauth2Token, nil
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", p.oauth2ClientID)
	form.Set("client_secret", p.oauth2ClientSecret)
	form.Set("scope", "api")

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
		return "", fmt.Errorf("oauth2 token endpoint returned %d", resp.StatusCode)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", err
	}

	p.oauth2Token = tokenResp.AccessToken
	p.oauth2TokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)

	return p.oauth2Token, nil
}

func (p *Provider) getItemByID(ctx context.Context, id string) (*VaultItem, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/object/item/%s", p.ApiUrl, id), nil)
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
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
