package restapi

import (
	"crypto/subtle"
	"errors"
	"net/http"
)

const KeyHeader = "x-api-key" // Header for API key

var (
	ErrInvalidApiKey     = errors.New("invalid api key")
	ErrInvalidAction     = errors.New("invalid action")
	ErrInvalidHTTPMethod = errors.New("invalid http method")
)

// ValidateApiKey checks if the provided API key matches the one in the request header.
func ValidateApiKey(r *http.Request, apiKey string) bool {
	if apiKey == "" {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(r.Header.Get(KeyHeader)), []byte(apiKey)) == 1
}
