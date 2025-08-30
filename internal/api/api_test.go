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

	validKey := appConfig.ApiSecret
	invalidKey := "invalid_key"

	testCases := []struct {
		name       string
		apiKey     string
		shouldPass bool
	}{
		{"Valid API Key", validKey, true},
		{"Invalid API Key", invalidKey, false},
		{"Missing API Key", "", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest("GET", "/v1/api", nil)
			if err != nil {
				t.Fatalf("Failed to create request: %v", err)
			}

			if tc.apiKey != "" {
				req.Header.Add(KeyHeader, tc.apiKey)
			}

			valid := ValidateApiKey(req, appConfig.ApiSecret)
			if valid != tc.shouldPass {
				t.Errorf("Expected validation to be %v, got %v", tc.shouldPass, valid)
			}
		})
	}
}
