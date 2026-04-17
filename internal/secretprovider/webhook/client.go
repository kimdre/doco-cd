package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"text/template"

	jmespath "github.com/jmespath/go-jmespath"
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

// ValueProvider provides generic access to remote secrets
// using a HTTP client for retrieval.
type ValueProvider struct {
	endpoint      *template.Template
	payload       *template.Template
	queryTpl      *template.Template
	query         *jmespath.JMESPath
	client        *http.Client
	customHeaders map[string]string
}

// NewValueProvider returns a new ValueProvider based on the given configuration.
// A malformed or incomplete configuration will yield an error.
func NewValueProvider(ctx context.Context, cfg *Config) (*ValueProvider, error) {
	rt, err := cfg.NewRoundTripperWithContext(ctx)
	if err != nil {
		return nil, err
	}

	result := &ValueProvider{
		client:        &http.Client{Transport: rt},
		customHeaders: cfg.CustomHeaders,
	}

	result.queryTpl, err = template.New("webhook-query").Parse(cfg.ResultJMESPath)
	if err != nil {
		return nil, err
	}

	result.query, err = jmespath.Compile(cfg.ResultJMESPath)
	if err != nil {
		return nil, err
	}

	result.endpoint, err = template.New("webhook-url").Parse(cfg.SiteUrl)
	if err != nil {
		return nil, err
	}

	if cfg.RequestBody != "" {
		result.payload, err = template.New("webhook-body").Parse(cfg.RequestBody)
		if err != nil {
			return nil, err
		}
	}

	return result, nil
}

// GetSecret fetches a single secret value from the remote endpoint using the
// provided identifier as template input for retrieval logic. The response is
// expected to be JSON encoded as it will be passed to a JMESPath evaluator
// in order to extract the resulting value.
func (p *ValueProvider) GetSecret(ctx context.Context, id string) (string, error) {
	req, query, err := p.newRequest(ctx, id)
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

	var data interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}

	result, err := query.Search(data)
	if err != nil {
		return "", err
	} else if value, ok := result.(string); ok {
		return value, nil
	}

	return "", fmt.Errorf("JMESPath query did not yield a string but a %T", result)
}

func (p *ValueProvider) newRequest(ctx context.Context, id string) (*http.Request, *jmespath.JMESPath, error) {
	var body io.Reader

	buf := new(bytes.Buffer)
	tplParams := map[string]string{
		"remoteRef": id,
	}

	if err := p.endpoint.Execute(buf, tplParams); err != nil {
		return nil, nil, err
	}

	url := buf.String()
	method := http.MethodGet

	if p.payload != nil {
		buf.Reset() // reuse buffer for payload rendering
		body = buf
		method = http.MethodPost

		if err := p.payload.Execute(buf, tplParams); err != nil {
			return nil, nil, err
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, nil, err
	}

	if p.payload != nil {
		req.Header.Set(HeaderContentType, ContentTypeJSON+"; charset=utf-8")
	}

	req.Header.Set(HeaderAccept, ContentTypeJSON)

	// Apply custom headers last so they can override defaults
	for key, value := range p.customHeaders {
		req.Header.Set(key, value)
	}

	// Render json_path template into a separate buffer, leaving the body buffer untouched
	jsonPathBuf := new(bytes.Buffer)
	if err := p.queryTpl.Execute(jsonPathBuf, tplParams); err != nil {
		return nil, nil, err
	}

	query, err := jmespath.Compile(jsonPathBuf.String())
	if err != nil {
		return nil, nil, err
	}

	return req, query, nil
}
