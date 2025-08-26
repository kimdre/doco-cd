package api

import "net/http"

const KeyHeader = "x-api-key" // Header for API key

// ValidateApiKey checks if the provided API key matches the one in the request header.
func ValidateApiKey(r *http.Request, apiKey string) bool {
	if apiKey == "" {
		return true
	}

	return r.Header.Get(KeyHeader) == apiKey
}
