package restapi

import (
	"net/http"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
)

func TestValidateApiKey(t *testing.T) {
	t.Parallel()

	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	testCases := []struct {
		name       string
		apiKey     string
		checkKey   string
		setHeader  bool
		shouldPass bool
	}{
		{"Valid API Key", appConfig.ApiSecret, appConfig.ApiSecret, true, true},
		{"Invalid API Key", appConfig.ApiSecret, "invalid_key", true, false},
		{"Missing API Key", appConfig.ApiSecret, "", false, false},
		{"Unset API Key", "", "api_key", true, false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequest("GET", "/v1/api", nil)
			if err != nil {
				t.Fatalf("Failed to create request: %v", err)
			}

			if tc.setHeader {
				req.Header.Add(KeyHeader, tc.checkKey)
			}

			valid := ValidateApiKey(req, tc.apiKey)
			if valid != tc.shouldPass {
				t.Errorf("Expected validation to be %v, got %v", tc.shouldPass, valid)
			}
		})
	}
}
