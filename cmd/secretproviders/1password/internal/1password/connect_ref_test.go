package onepassword

import "testing"

func TestParseOPSecretReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		ref       string
		wantVault string
		wantItem  string
		wantSect  string
		wantField string
		wantAttr  string
		wantErr   bool
	}{
		{
			name:      "item field",
			ref:       "op://MyVault/MyItem/MyField",
			wantVault: "MyVault",
			wantItem:  "MyItem",
			wantField: "MyField",
		},
		{
			name:      "section field",
			ref:       "op://MyVault/MyItem/MySection/MyField",
			wantVault: "MyVault",
			wantItem:  "MyItem",
			wantSect:  "MySection",
			wantField: "MyField",
		},
		{
			name:      "otp attribute",
			ref:       "op://MyVault/MyItem/one-time%20password?attribute=otp",
			wantVault: "MyVault",
			wantItem:  "MyItem",
			wantField: "one-time password",
			wantAttr:  "otp",
		},
		{
			name:    "invalid scheme",
			ref:     "http://MyVault/MyItem/MyField",
			wantErr: true,
		},
		{
			name:    "invalid path length",
			ref:     "op://MyVault/MyItem",
			wantErr: true,
		},
		{
			name:    "unsupported attribute",
			ref:     "op://MyVault/MyItem/MyField?attribute=foo",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseOPSecretReference(tc.ref)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected parse error")
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got.Vault != tc.wantVault || got.Item != tc.wantItem || got.Section != tc.wantSect || got.Field != tc.wantField || got.Attribute != tc.wantAttr {
				t.Fatalf("got %+v", got)
			}
		})
	}
}
