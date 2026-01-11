package restapi

import (
	"errors"
	"net/http"
)

const KeyHeader = "x-api-key" // Header for API key

var (
	ErrInvalidApiKey = errors.New("invalid api key")
	ErrInvalidAction = errors.New("invalid action")
)

// ValidateApiKey checks if the provided API key matches the one in the request header.
func ValidateApiKey(r *http.Request, apiKey string) bool {
	if apiKey == "" {
		return true
	}

	return r.Header.Get(KeyHeader) == apiKey
}
