package secrets

import "testing"

func TestNewBitwardenProvider(t *testing.T) {
	testCases := []struct {
		name        string
		apiUrl      string
		identityURL string
		accessToken string
		expectError bool
	}{
		{
			name:        "Valid parameters",
			apiUrl:      "https://api.bitwarden.com",
			identityURL: "https://identity.bitwarden.com",
			accessToken: "valid-access-token", // Replace with a valid token for real testing
			expectError: false,
		},
		{
			name:        "Invalid API URL",
			apiUrl:      "invalid-url",
			identityURL: "https://identity.bitwarden.com",
			accessToken: "valid-access-token",
			expectError: true,
		},
		{
			name:        "Empty Access Token",
			apiUrl:      "https://api.bitwarden.com",
			identityURL: "https://identity.bitwarden.com",
			accessToken: "",
			expectError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			provider, err := NewBitwardenSecretsManagerProvider(tc.apiUrl, tc.identityURL, tc.accessToken)
			if tc.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
			} else {
				t.Cleanup(func() {
					provider.Close()
				})

				if err != nil {
					t.Errorf("Did not expect error but got: %v", err)
				}

				if provider == nil {
					t.Fatal("Expected provider to be non-nil")
				}

				if provider.Client == nil {
					t.Errorf("Expected provider.Client to be non-nil")
				}
			}
		})
	}
}
