package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"

	"github.com/jmespath/go-jmespath"
)

const (
	Name = "webhook"

	// HeaderContentType is the header to include in requests to remote endpoints.
	HeaderContentType = "Content-Type"
	// HeaderAccept is the header to include in requests to remote endpoints.
	HeaderAccept = "Accept"
	// ContentTypeJSON is the header value to send to remote secret endpoints.
	ContentTypeJSON = "application/json"
)

var (
	ErrLegacyWebhookRefNotSupported = errors.New("webhook provider no longer supports string external_secrets references; use object format with store_ref and remote_ref")
	ErrUnknownStoreRef              = errors.New("unknown webhook secret store reference")
	ErrMissingRemoteRefField        = errors.New("missing required remote_ref field")
)

type SecretRefPayload struct {
	StoreRef  string                 `json:"store_ref"`
	RemoteRef map[string]interface{} `json:"remote_ref"`
}

// ValueProvider provides generic access to remote secrets
// using a HTTP client for retrieval.
type ValueProvider struct {
	client *http.Client
	stores map[string]*Store
	auth   map[string]string
}

// NewValueProvider returns a new ValueProvider based on the given configuration.
// A malformed or incomplete configuration will yield an error.
func NewValueProvider(ctx context.Context, cfg *Config) (*ValueProvider, error) {
	rt, err := cfg.NewRoundTripperWithContext(ctx)
	if err != nil {
		return nil, err
	}

	result := &ValueProvider{
		client: &http.Client{Transport: rt},
		stores: cfg.Stores,
		auth:   cfg.Auth,
	}

	return result, nil
}

// GetSecret fetches a single secret value from a webhook store reference.
func (p *ValueProvider) GetSecret(ctx context.Context, id string) (string, error) {
	payload, err := parseSecretRefPayload(id)
	if err != nil {
		return "", err
	}

	store, ok := p.stores[payload.StoreRef]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownStoreRef, payload.StoreRef)
	}

	if err := validateRemoteRefFields(store, payload.RemoteRef); err != nil {
		return "", err
	}

	req, renderedJSONPath, err := p.newRequest(ctx, store, payload.RemoteRef)
	if err != nil {
		return "", err
	}

	resp, err := p.client.Do(req) // #nosec G704
	if err != nil {
		return "", err
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("webhook request failed with status %d", resp.StatusCode)
	}

	var data interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}

	query, err := jmespath.Compile(renderedJSONPath)
	if err != nil {
		return "", err
	}

	result, err := query.Search(data)
	if err != nil {
		return "", err
	} else if value, ok := result.(string); ok {
		return value, nil
	}

	if values, ok := result.([]interface{}); ok && len(values) > 0 {
		if value, ok := values[0].(string); ok {
			return value, nil
		}
	}

	return "", fmt.Errorf("JMESPath query did not yield a string but a %T", result)
}

func (p *ValueProvider) newRequest(ctx context.Context, store *Store, remoteRef map[string]interface{}) (*http.Request, string, error) {
	var body io.Reader

	buf := new(bytes.Buffer)
	tplParams := map[string]interface{}{
		"remote_ref": remoteRef,
		"auth":       p.auth,
	}

	if err := store.urlTemplate.Execute(buf, tplParams); err != nil {
		return nil, "", err
	}

	url := buf.String()
	method := store.Method

	if store.bodyTemplate != nil {
		buf.Reset() // reuse buffer for payload rendering
		body = buf

		if err := store.bodyTemplate.Execute(buf, tplParams); err != nil {
			return nil, "", err
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, "", err
	}

	if store.bodyTemplate != nil {
		req.Header.Set(HeaderContentType, ContentTypeJSON+"; charset=utf-8")
	}

	req.Header.Set(HeaderAccept, ContentTypeJSON)

	for key, tpl := range store.headerTemplates {
		buf.Reset()

		if err := tpl.Execute(buf, tplParams); err != nil {
			return nil, "", err
		}

		req.Header.Set(key, buf.String())
	}

	buf.Reset()

	if err := store.jsonPathTemplate.Execute(buf, tplParams); err != nil {
		return nil, "", err
	}

	renderedJSONPath := buf.String()
	if renderedJSONPath == "" {
		return nil, "", fmt.Errorf("store %q rendered an empty jsonPath", store.Name)
	}

	return req, renderedJSONPath, nil
}

func parseSecretRefPayload(raw string) (*SecretRefPayload, error) {
	var payload SecretRefPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, ErrLegacyWebhookRefNotSupported
	}


	if payload.StoreRef == "" {
		return nil, fmt.Errorf("%w: missing store_ref", ErrLegacyWebhookRefNotSupported)
	}

	if payload.RemoteRef == nil {
		payload.RemoteRef = map[string]interface{}{}
	}

	return &payload, nil
}

func validateRemoteRefFields(store *Store, remoteRef map[string]interface{}) error {
	missing := make([]string, 0)

	for _, field := range store.requiredRemoteRefFields() {
		if _, ok := remoteRef[field]; !ok {
			missing = append(missing, field)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("%w for store %q: %v", ErrMissingRemoteRefField, store.Name, missing)
	}

	return nil
}
