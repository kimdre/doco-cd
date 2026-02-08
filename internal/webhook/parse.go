package webhook

import (
	"errors"
	"fmt"
	"io"
	"net/http"
)

var (
	ErrInvalidHTTPMethod = errors.New("invalid http method")
	ErrParsingPayload    = errors.New("failed to parse payload")
)

// Parse parses the payload and returns the parsed payload data.
func Parse(r *http.Request, secretKey string) (ScmProvider, ParsedPayload, error) {
	if r.Body == nil {
		return Unknown, ParsedPayload{}, fmt.Errorf("%w: request body is empty", ErrParsingPayload)
	}

	defer func() {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}()

	if r.Method != http.MethodPost {
		return Unknown, ParsedPayload{}, ErrInvalidHTTPMethod
	}

	payload, err := io.ReadAll(r.Body)
	if err != nil || len(payload) == 0 {
		return Unknown, ParsedPayload{}, err
	}

	provider, err := verifyProviderSecret(r, payload, secretKey)
	if err != nil {
		return Unknown, ParsedPayload{}, err
	}

	parsedPayload, err := parsePayload(payload, provider)
	if err != nil {
		return 0, ParsedPayload{}, err
	}

	return provider, parsedPayload, nil
}
