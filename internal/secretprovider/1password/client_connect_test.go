package onepassword

import (
	"strings"
	"testing"

	connectonepassword "github.com/1Password/connect-sdk-go/onepassword"
)

type mockConnectClient struct {
	item           *connectonepassword.Item
	err            error
	calls          int
	lastItemQuery  string
	lastVaultQuery string
}

func (m *mockConnectClient) GetItem(itemQuery, vaultQuery string) (*connectonepassword.Item, error) {
	m.calls++
	m.lastItemQuery = itemQuery
	m.lastVaultQuery = vaultQuery

	if m.err != nil {
		return nil, m.err
	}

	return m.item, nil
}

func TestProvider_ResolveConnectSecret_FieldSelection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		uri           string
		item          *connectonepassword.Item
		expected      string
		expectErr     bool
		errContains   string
		expectedItem  string
		expectedVault string
	}{
		{
			name: "section matching selects field from matching section",
			uri:  "op://TestVault/TestItem/Production/password",
			item: &connectonepassword.Item{
				Fields: []*connectonepassword.ItemField{
					{Label: "password", Value: "dev-secret", Section: &connectonepassword.ItemSection{Label: "Development"}},
					{Label: "password", Value: "prod-secret", Section: &connectonepassword.ItemSection{Label: "Production"}},
				},
			},
			expected:      "prod-secret",
			expectedItem:  "TestItem",
			expectedVault: "TestVault",
		},
		{
			name: "otp attribute returns totp value",
			uri:  "op://TestVault/TestItem/one-time%20password?attribute=otp",
			item: &connectonepassword.Item{
				Fields: []*connectonepassword.ItemField{
					{Label: "one-time password", Value: "ignored", TOTP: "123456"},
				},
			},
			expected:      "123456",
			expectedItem:  "TestItem",
			expectedVault: "TestVault",
		},
		{
			name: "otp attribute fails when totp is missing",
			uri:  "op://TestVault/TestItem/one-time%20password?attribute=otp",
			item: &connectonepassword.Item{
				Fields: []*connectonepassword.ItemField{
					{Label: "one-time password", Value: "plain-value"},
				},
			},
			expectErr:     true,
			errContains:   "secret field not found",
			expectedItem:  "TestItem",
			expectedVault: "TestVault",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mock := &mockConnectClient{item: tc.item}
			provider := &Provider{connectClient: mock}

			got, err := provider.resolveConnectSecret(t.Context(), tc.uri)
			if tc.expectErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}

				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("expected error to contain %q, got %v", tc.errContains, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}

				if got != tc.expected {
					t.Fatalf("expected %q, got %q", tc.expected, got)
				}
			}

			if mock.calls != 1 {
				t.Fatalf("expected connect client to be called once, got %d", mock.calls)
			}

			if mock.lastItemQuery != tc.expectedItem {
				t.Fatalf("expected item query %q, got %q", tc.expectedItem, mock.lastItemQuery)
			}

			if mock.lastVaultQuery != tc.expectedVault {
				t.Fatalf("expected vault query %q, got %q", tc.expectedVault, mock.lastVaultQuery)
			}
		})
	}
}
