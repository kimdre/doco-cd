package onepassword

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

const (
	Name = "1password"
)

var ErrInvalidClientID = errors.New("invalid client id")

type authMode string

const (
	authModeServiceAccount authMode = "service_account"
	authModeConnect        authMode = "connect"
)

type Provider struct {
	mode authMode

	serviceClient *serviceAccountClient
	connectClient connectClient

	accessToken  string
	connectHost  string
	connectToken string
	version      string

	cacheEnabled bool
	cacheTTL     time.Duration
	cacheMaxSize int
	cacheMu      sync.RWMutex
	cache        map[string]cacheEntry
	cacheOrder   *list.List
	cacheNodes   map[string]*list.Element
}

type cacheEntry struct {
	value     string
	expiresAt time.Time
}

func (p *Provider) Name() string {
	return Name
}

// NewProvider creates a new Provider instance for 1Password using Connect if configured, otherwise service-account auth.
func NewProvider(ctx context.Context, cfg *Config, version string) (*Provider, error) {
	provider := &Provider{
		accessToken:  cfg.AccessToken,
		connectHost:  cfg.ConnectHost,
		connectToken: cfg.ConnectToken,
		version:      version,
		cacheEnabled: cfg.CacheEnabled,
		cacheTTL:     cfg.CacheTTL,
		cacheMaxSize: cfg.CacheMaxSize,
		cache:        make(map[string]cacheEntry),
		cacheOrder:   list.New(),
		cacheNodes:   make(map[string]*list.Element),
	}

	if cfg.UseConnect() {
		provider.mode = authModeConnect
		provider.cacheEnabled = false

		if err := provider.initializeConnectClient(); err != nil {
			return nil, err
		}

		return provider, nil
	}

	provider.mode = authModeServiceAccount

	if err := provider.initializeServiceAccountClient(ctx); err != nil {
		return nil, err
	}

	return provider, nil
}

func (p *Provider) getCachedSecret(uri string) (string, bool) {
	if !p.cacheEnabled {
		return "", false
	}

	now := time.Now()

	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()

	entry, ok := p.cache[uri]

	if !ok {
		return "", false
	}

	if now.After(entry.expiresAt) {
		if current, exists := p.cache[uri]; exists && now.After(current.expiresAt) {
			p.deleteCacheEntry(uri)
		}

		return "", false
	}

	p.touchCacheEntry(uri)

	return entry.value, true
}

func (p *Provider) setCachedSecret(uri, value string) {
	if !p.cacheEnabled {
		return
	}

	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()

	if p.cache == nil {
		p.cache = make(map[string]cacheEntry)
	}

	if p.cacheOrder == nil {
		p.cacheOrder = list.New()
	}

	if p.cacheNodes == nil {
		p.cacheNodes = make(map[string]*list.Element)
	}

	if _, exists := p.cache[uri]; !exists && p.cacheMaxSize > 0 && len(p.cache) >= p.cacheMaxSize {
		p.evictLeastRecentlyUsed()
	}

	p.cache[uri] = cacheEntry{value: value, expiresAt: time.Now().Add(p.cacheTTL)}
	p.touchCacheEntry(uri)
}

func (p *Provider) touchCacheEntry(uri string) {
	if p.cacheOrder == nil {
		p.cacheOrder = list.New()
	}

	if p.cacheNodes == nil {
		p.cacheNodes = make(map[string]*list.Element)
	}

	if node, ok := p.cacheNodes[uri]; ok {
		p.cacheOrder.MoveToFront(node)

		return
	}

	p.cacheNodes[uri] = p.cacheOrder.PushFront(uri)
}

func (p *Provider) deleteCacheEntry(uri string) {
	delete(p.cache, uri)

	if node, ok := p.cacheNodes[uri]; ok {
		p.cacheOrder.Remove(node)
		delete(p.cacheNodes, uri)
	}
}

func (p *Provider) evictLeastRecentlyUsed() {
	if p.cacheOrder == nil {
		return
	}

	leastRecent := p.cacheOrder.Back()
	if leastRecent == nil {
		return
	}

	uri, ok := leastRecent.Value.(string)
	if !ok {
		p.cacheOrder.Remove(leastRecent)

		return
	}

	p.deleteCacheEntry(uri)
}

// GetSecret retrieves a secret value from 1Password using the provided URI.
func (p *Provider) GetSecret(ctx context.Context, uri string) (string, error) {
	if cachedSecret, ok := p.getCachedSecret(uri); ok {
		return cachedSecret, nil
	}

	secret, err := p.resolveSecret(ctx, uri)
	if err != nil {
		return "", err
	}

	p.setCachedSecret(uri, secret)

	return secret, nil
}

// GetSecrets retrieves multiple secrets from 1Password using the provided list of secret references.
func (p *Provider) GetSecrets(ctx context.Context, uris []string) (map[string]string, error) {
	result := make(map[string]string, len(uris))
	missing := make([]string, 0, len(uris))

	for _, uri := range uris {
		if cachedSecret, ok := p.getCachedSecret(uri); ok {
			result[uri] = cachedSecret
			continue
		}

		missing = append(missing, uri)
	}

	if len(missing) == 0 {
		return result, nil
	}

	resolved, err := p.resolveSecrets(ctx, missing)
	if err != nil {
		return nil, err
	}

	for uri, secret := range resolved {
		result[uri] = secret
		p.setCachedSecret(uri, secret)
	}

	return result, nil
}

// ResolveSecretReferences resolves the provided map of environment variable names to secret IDs
// by fetching the corresponding secret values from the secret provider.
func (p *Provider) ResolveSecretReferences(ctx context.Context, secrets map[string]string) (secrettypes.ResolvedSecrets, error) {
	ids := make([]string, 0, len(secrets))
	for _, id := range secrets {
		ids = append(ids, id)
	}

	resolved, err := p.GetSecrets(ctx, ids)
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
func (p *Provider) Close() {
	// No resources to clean up for 1Password client
}
