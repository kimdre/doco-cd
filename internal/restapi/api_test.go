package restapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRoleAllows(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		role        Role
		required    Role
		wantAllowed bool
	}{
		{RoleRead, RoleRead, true},
		{RoleRead, RoleInspect, false},
		{RoleRead, RoleWrite, false},
		{RoleRead, RoleFull, false},
		{RoleInspect, RoleRead, true},
		{RoleInspect, RoleInspect, true},
		{RoleInspect, RoleWrite, false},
		{RoleInspect, RoleFull, false},
		{RoleWrite, RoleRead, true},
		{RoleWrite, RoleInspect, true},
		{RoleWrite, RoleWrite, true},
		{RoleWrite, RoleFull, false},
		{RoleFull, RoleRead, true},
		{RoleFull, RoleInspect, true},
		{RoleFull, RoleWrite, true},
		{RoleFull, RoleFull, true},
		{"unknown", RoleRead, false},
		{RoleFull, "unknown", false},
		{"", "", false},
	} {
		t.Run(string(testCase.role)+"/"+string(testCase.required), func(t *testing.T) {
			t.Parallel()

			if got := testCase.role.Allows(testCase.required); got != testCase.wantAllowed {
				t.Fatalf("Allows(%q) = %t, want %t", testCase.required, got, testCase.wantAllowed)
			}
		})
	}
}

func TestKeyringAuthenticate(t *testing.T) {
	t.Parallel()

	keys := Keyring{
		{Key: "reader", Role: RoleRead},
		{Key: "inspector", Role: RoleInspect},
		{Key: "writer", Role: RoleWrite},
		{Key: "admin", Role: RoleFull},
		{Key: "invalid-role", Role: "unknown"},
		{Key: "", Role: RoleFull},
	}

	for _, testCase := range []struct {
		name string
		key  string
		role Role
		ok   bool
	}{
		{"read", "reader", RoleRead, true},
		{"inspect", "inspector", RoleInspect, true},
		{"write", "writer", RoleWrite, true},
		{"full", "admin", RoleFull, true},
		{"missing", "", "", false},
		{"incorrect", "other", "", false},
		{"invalid role", "invalid-role", "unknown", false},
		{"partial match", "read", "", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set(KeyHeader, testCase.key)

			role, ok := keys.Authenticate(request)
			if role != testCase.role || ok != testCase.ok {
				t.Fatalf("Authenticate() = (%q, %t), want (%q, %t)", role, ok, testCase.role, testCase.ok)
			}
		})
	}
}

func TestKeyringValidate(t *testing.T) {
	t.Parallel()

	const secret = "sensitive-test-value"

	for _, testCase := range []struct {
		name    string
		keys    Keyring
		wantErr string
	}{
		{name: "empty", keys: nil},
		{name: "valid", keys: Keyring{{Key: secret, Role: RoleRead}, {Key: "inspector", Role: RoleInspect}, {Key: "other", Role: RoleWrite}}},
		{name: "valid spaces and tabs", keys: Keyring{{Key: "key with\ttab", Role: RoleRead}}},
		{name: "valid non-ASCII", keys: Keyring{{Key: "key-\u00e4", Role: RoleRead}}},
		{name: "empty key", keys: Keyring{{Role: RoleRead}}, wantErr: "key must not be empty"},
		{name: "whitespace key", keys: Keyring{{Key: "  ", Role: RoleRead}}, wantErr: "key must not be empty"},
		{name: "leading space", keys: Keyring{{Key: " " + secret, Role: RoleRead}}, wantErr: "valid HTTP header"},
		{name: "trailing space", keys: Keyring{{Key: secret + " ", Role: RoleRead}}, wantErr: "valid HTTP header"},
		{name: "leading tab", keys: Keyring{{Key: "\t" + secret, Role: RoleRead}}, wantErr: "valid HTTP header"},
		{name: "trailing tab", keys: Keyring{{Key: secret + "\t", Role: RoleRead}}, wantErr: "valid HTTP header"},
		{name: "newline", keys: Keyring{{Key: secret + "\nother", Role: RoleRead}}, wantErr: "valid HTTP header"},
		{name: "carriage return", keys: Keyring{{Key: secret + "\rother", Role: RoleRead}}, wantErr: "valid HTTP header"},
		{name: "null", keys: Keyring{{Key: secret + "\x00other", Role: RoleRead}}, wantErr: "valid HTTP header"},
		{name: "delete", keys: Keyring{{Key: secret + "\x7fother", Role: RoleRead}}, wantErr: "valid HTTP header"},
		{name: "missing role", keys: Keyring{{Key: secret}}, wantErr: "role must be"},
		{name: "invalid role", keys: Keyring{{Key: secret, Role: "invalid"}}, wantErr: "role must be"},
		{name: "duplicate key", keys: Keyring{{Key: secret, Role: RoleRead}, {Key: secret, Role: RoleFull}}, wantErr: "duplicate key"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			err := testCase.keys.Validate()
			if testCase.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("Validate() error = %v, want %q", err, testCase.wantErr)
			}

			if strings.Contains(err.Error(), secret) {
				t.Fatal("Validate() exposed a key in its error")
			}
		})
	}
}
