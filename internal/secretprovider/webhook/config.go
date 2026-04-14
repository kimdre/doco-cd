package webhook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	prococo "github.com/prometheus/common/config"
	"go.yaml.in/yaml/v3"

	"github.com/kimdre/doco-cd/internal/config"
)

type Config struct {
	StoresYAML     string `env:"SECRET_PROVIDER_WEBHOOK_STORES"`
	StoresYAMLFile string `env:"SECRET_PROVIDER_WEBHOOK_STORES_FILE,file"`

	AuthUsername string `env:"SECRET_PROVIDER_AUTH_USERNAME"`
	AuthPassword string `env:"SECRET_PROVIDER_AUTH_PASSWORD"`
	AuthToken    string `env:"SECRET_PROVIDER_AUTH_TOKEN"`
	AuthAPIKey   string `env:"SECRET_PROVIDER_AUTH_APIKEY"`

	Stores map[string]*Store `env:"-"`
	Auth   map[string]string `env:"-"`
}

// GetConfig retrieves and parses the configuration for the Webhook Secrets Provider from environment variables.
func GetConfig() (*Config, error) {
	cfg := Config{}

	mappings := []config.EnvVarFileMapping{
		{EnvName: "SECRET_PROVIDER_WEBHOOK_STORES", EnvValue: &cfg.StoresYAML, FileValue: &cfg.StoresYAMLFile, AllowUnset: false},
	}

	err := config.ParseConfigFromEnv(&cfg, &mappings)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", config.ErrParseConfigFailed, err)
	}

	stores, err := parseStoresYAML(cfg.StoresYAML)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to parse webhook stores: %w", config.ErrParseConfigFailed, err)
	}

	cfg.Stores = stores
	cfg.Auth = map[string]string{
		"username": cfg.AuthUsername,
		"password": cfg.AuthPassword,
		"token":    cfg.AuthToken,
		"apiKey":   cfg.AuthAPIKey,
	}

	return &cfg, nil
}

func (c *Config) NewRoundTripperWithContext(ctx context.Context) (http.RoundTripper, error) {
	httpcfg := prococo.HTTPClientConfig{}
	if err := httpcfg.Validate(); err != nil {
		return nil, err
	}

	return prococo.NewRoundTripperFromConfigWithContext(ctx, httpcfg,
		"secretprovider-webhook", prococo.WithUserAgent(config.AppName+"/"+config.AppVersion))
}

func parseStoresYAML(input string) (map[string]*Store, error) {
	dec := yaml.NewDecoder(strings.NewReader(input))
	stores := make(map[string]*Store)

	for {
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			if err == io.EOF {
				break
			}

			return nil, err
		}

		if node.Kind == 0 {
			continue
		}

		if err := parseStoreDocument(&node, stores); err != nil {
			return nil, err
		}
	}

	if len(stores) == 0 {
		return nil, errors.New("no stores found in webhook store configuration")
	}

	funcMap := BuildTemplateFuncMap()
	for _, store := range stores {
		if err := store.validateAndPrepare(funcMap); err != nil {
			return nil, err
		}
	}

	return stores, nil
}

func parseStoreDocument(node *yaml.Node, stores map[string]*Store) error {
	type genericDoc struct {
		Stores yaml.Node `yaml:"stores"`
	}

	var doc genericDoc
	if err := node.Decode(&doc); err != nil {
		return err
	}

	if doc.Stores.Kind == yaml.MappingNode {
		m := make(map[string]*Store)
		if err := doc.Stores.Decode(&m); err != nil {
			return err
		}

		for name, store := range m {
			if store == nil {
				return fmt.Errorf("store %q must not be null", name)
			}

			if store.Name == "" {
				store.Name = name
			}

			if _, exists := stores[store.Name]; exists {
				return fmt.Errorf("duplicate store %q", store.Name)
			}

			stores[store.Name] = store
		}

		return nil
	}

	if doc.Stores.Kind == yaml.SequenceNode {
		var list []*Store
		if err := doc.Stores.Decode(&list); err != nil {
			return err
		}

		for _, store := range list {
			if store == nil {
				return errors.New("store entry must not be null")
			}

			if store.Name == "" {
				return errors.New("store name is required when using list format")
			}

			if _, exists := stores[store.Name]; exists {
				return fmt.Errorf("duplicate store %q", store.Name)
			}

			stores[store.Name] = store
		}

		return nil
	}

	var single Store
	if err := node.Decode(&single); err != nil {
		return err
	}

	if single.Name == "" {
		return nil
	}

	if _, exists := stores[single.Name]; exists {
		return fmt.Errorf("duplicate store %q", single.Name)
	}

	stores[single.Name] = &single

	return nil
}
