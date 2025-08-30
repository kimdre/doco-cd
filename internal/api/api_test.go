package api

import (
	"net/http"
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
)

func TestValidateApiKey(t *testing.T) {
	appConfig, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	testCases := []struct {
		name       string
		apiKey     string
		checkKey   string
		shouldPass bool
	}{
		{"Valid API Key", appConfig.ApiSecret, appConfig.ApiSecret, true},
		{"Invalid API Key", appConfig.ApiSecret, "invalid_key", false},
		{"Missing API Key", appConfig.ApiSecret, "", false},
		{"Unset API Key", "", "", true}, // If no API key is set in config, all requests should pass
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest("GET", "/v1/api", nil)
			if err != nil {
				t.Fatalf("Failed to create request: %v", err)
			}

			if tc.apiKey != "" {
				req.Header.Add(KeyHeader, tc.checkKey)
			}

			valid := ValidateApiKey(req, tc.apiKey)
			if valid != tc.shouldPass {
				t.Errorf("Expected validation to be %v, got %v", tc.shouldPass, valid)
			}
		})
	}
}
