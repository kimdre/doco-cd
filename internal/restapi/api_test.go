package restapi

import (
	"net/http"
	"testing"
)

func TestValidateApiKey(t *testing.T) {
	t.Parallel()

	const apiKey = "test_api_secret"

	testCases := []struct {
		name       string
		apiKey     string
		checkKey   string
		setHeader  bool
		shouldPass bool
	}{
		{
			name:       "Valid API Key",
			apiKey:     apiKey,
			checkKey:   apiKey,
			setHeader:  true,
			shouldPass: true,
		},
		{
			name:       "Invalid API Key",
			apiKey:     apiKey,
			checkKey:   "test_apiSecret2",
			setHeader:  true,
			shouldPass: false,
		},
		{
			name:       "Missing API Key",
			apiKey:     apiKey,
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
