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
		{
			name:       "Valid API Key",
			apiKey:     appConfig.ApiSecret,
			checkKey:   appConfig.ApiSecret,
			setHeader:  true,
			shouldPass: true,
		},
		{
			name:       "Invalid API Key",
			apiKey:     appConfig.ApiSecret,
			checkKey:   "test_apiSecret2",
			setHeader:  true,
			shouldPass: false,
		},
		{
			name:       "Missing API Key",
			apiKey:     appConfig.ApiSecret,
			setHeader:  false,
			shouldPass: false,
		},
		{
			name:       "Unset API Key",
			apiKey:     "",
			checkKey:   "",
			setHeader:  true,
			shouldPass: false,
		},
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
