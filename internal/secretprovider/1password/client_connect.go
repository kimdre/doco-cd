package onepassword

import (
	"context"
	"fmt"
	"sync"

	connectsdk "github.com/1Password/connect-sdk-go/connect"
	connectonepassword "github.com/1Password/connect-sdk-go/onepassword"
	"github.com/opentracing/opentracing-go"
	"golang.org/x/sync/errgroup"
)

const defaultMaxConcurrentSecrets = 10

var connectGlobalTracerInit sync.Once

type connectClient interface {
	GetItem(itemQuery, vaultQuery string) (*connectonepassword.Item, error)
}

type connectSDKClient struct {
	inner connectsdk.Client
}

func (c connectSDKClient) GetItem(itemQuery, vaultQuery string) (*connectonepassword.Item, error) {
	return c.inner.GetItem(itemQuery, vaultQuery)
}

func (p *Provider) initializeConnectClient() error {
	ensureConnectSDKGlobalTracerDisabled()

	p.connectClient = connectSDKClient{inner: connectsdk.NewClientWithUserAgent(p.connectHost, p.connectToken, "doco-cd/"+p.version)}

	return nil
}

func ensureConnectSDKGlobalTracerDisabled() {
	connectGlobalTracerInit.Do(func() {
		// Disable global tracing so the Connect SDK cannot initialize Jaeger tracing.
		opentracing.SetGlobalTracer(opentracing.NoopTracer{})
	})
}

func (p *Provider) resolveConnectSecret(_ context.Context, uri string) (string, error) {
	ref, err := ParseOPSecretReference(uri)
	if err != nil {
		return "", err
	}

	item, err := p.connectClient.GetItem(ref.Item, ref.Vault)
	if err != nil {
		return "", err
	}

	value, ok := findConnectFieldValue(item, ref)
	if !ok {
		return "", fmt.Errorf("secret field not found for reference: %s", uri)
	}

	return value, nil
}

func (p *Provider) resolveConnectSecrets(ctx context.Context, uris []string) (map[string]string, error) {
	if len(uris) == 0 {
		return make(map[string]string), nil
	}

	result := make(map[string]string, len(uris))

	var resultMutex sync.Mutex

	eg, egCtx := errgroup.WithContext(ctx)
	eg.SetLimit(defaultMaxConcurrentSecrets)

	for _, uri := range uris {
		eg.Go(func() error {
			secret, err := p.resolveConnectSecret(egCtx, uri)
			if err != nil {
				return fmt.Errorf("failed to resolve secret for %s: %w", uri, err)
			}

			resultMutex.Lock()
			result[uri] = secret
			resultMutex.Unlock()

			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	return result, nil
}

func findConnectFieldValue(item *connectonepassword.Item, ref *OPSecretReference) (string, bool) {
	selector := ref.Field
	if ref.Section != "" {
		selector = ref.Section + "." + ref.Field
	}

	for _, field := range item.Fields {
		if field == nil {
			continue
		}

		if field.Label != ref.Field {
			continue
		}

		if ref.Section != "" {
			if field.Section == nil || field.Section.Label != ref.Section {
				continue
			}
		}

		if ref.Attribute == "otp" {
			if field.TOTP == "" {
				return "", false
			}

			return field.TOTP, true
		}

		return field.Value, true
	}

	if ref.Attribute == "otp" {
		return "", false
	}

	if value := item.GetValue(selector); value != "" {
		return value, true
	}

	return "", false
}
