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
func Parse(r *http.Request, secretKey string) (ParsedPayload, error) {
	if r.Body == nil {
		return ParsedPayload{}, fmt.Errorf("%w: request body is empty", ErrParsingPayload)
	}

	defer func() {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}()

	if r.Method != http.MethodPost {
		return ParsedPayload{}, ErrInvalidHTTPMethod
	}

	payload, err := io.ReadAll(r.Body)
	if err != nil || len(payload) == 0 {
		return ParsedPayload{}, err
	}

	provider, err := verifyProviderSecret(r, payload, secretKey)
	if err != nil {
		return ParsedPayload{}, err
	}

	return parsePayload(payload, provider)
}
