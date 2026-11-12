package restapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRolePermissions(t *testing.T) {
	t.Parallel()

	permissions := []Permission{
		PermissionHealthRead,
		PermissionRunsRead,
		PermissionJobsRead,
		PermissionSyncWindowsRead,
		PermissionProjectsInspect,
		PermissionStacksInspect,
		PermissionJobsTrigger,
		PermissionProjectsControl,
		PermissionStacksControl,
		PermissionPollTrigger,
		PermissionMirrorsCompact,
		PermissionProjectsDelete,
		PermissionStacksDelete,
	}

	for _, testCase := range []struct {
		role    Role
		allowed []Permission
	}{
		{
			RoleRead,
			[]Permission{PermissionHealthRead, PermissionRunsRead, PermissionJobsRead, PermissionSyncWindowsRead},
		},
		{
			RoleInspect,
			[]Permission{
				PermissionHealthRead, PermissionRunsRead, PermissionJobsRead, PermissionSyncWindowsRead,
				PermissionProjectsInspect, PermissionStacksInspect,
			},
		},
		{
			RoleWrite,
			[]Permission{
				PermissionHealthRead, PermissionRunsRead, PermissionJobsRead, PermissionSyncWindowsRead,
				PermissionProjectsInspect, PermissionStacksInspect, PermissionJobsTrigger,
				PermissionProjectsControl, PermissionStacksControl, PermissionPollTrigger,
				PermissionMirrorsCompact,
			},
		},
		{
			RoleFull,
			permissions,
		},
	} {
		t.Run(string(testCase.role), func(t *testing.T) {
			allowed := make(map[Permission]bool, len(testCase.allowed))
			for _, permission := range testCase.allowed {
				allowed[permission] = true
			}

			for _, permission := range permissions {
				if got := testCase.role.Allows(permission); got != allowed[permission] {
					t.Errorf("Allows(%q) = %t, want %t", permission, got, allowed[permission])
				}
			}
		})
	}

	if Role("unknown").Valid() || Role("").Valid() {
		t.Fatal("unknown role is valid")
	}

	if Role("unknown").Allows(PermissionRunsRead) || Role("").Allows("") {
		t.Fatal("unknown role grants a permission")
	}

	if RoleFull.Allows(Permission("unknown")) {
		t.Fatal("unknown permission is allowed")
	}

	for _, testCase := range []struct {
		permission Permission
		wantRole   Role
	}{
		{PermissionHealthRead, RoleRead},
		{PermissionRunsRead, RoleRead},
		{PermissionJobsRead, RoleRead},
		{PermissionSyncWindowsRead, RoleRead},
		{PermissionProjectsInspect, RoleInspect},
		{PermissionStacksInspect, RoleInspect},
		{PermissionJobsTrigger, RoleWrite},
		{PermissionProjectsControl, RoleWrite},
		{PermissionStacksControl, RoleWrite},
		{PermissionPollTrigger, RoleWrite},
		{PermissionMirrorsCompact, RoleWrite},
		{PermissionProjectsDelete, RoleFull},
		{PermissionStacksDelete, RoleFull},
	} {
		if got, ok := MinimumRole(testCase.permission); !ok || got != testCase.wantRole {
			t.Errorf("MinimumRole(%q) = (%q, %t), want (%q, true)", testCase.permission, got, ok, testCase.wantRole)
		}
	}

	if _, ok := MinimumRole(Permission("unknown")); ok {
		t.Fatal("unknown permission has a minimum role")
	}
}

func TestKeyringAuthenticate(t *testing.T) {
	t.Parallel()

	keys := Keyring{
		{Key: "reader", Role: RoleRead},
		{Key: "inspector", Role: RoleInspect},
		{Key: "writer", Role: RoleWrite},
		{Key: "admin", Role: RoleFull},
		{Key: "poller", Permissions: []Permission{PermissionPollTrigger, PermissionRunsRead}},
		{Key: "invalid-role", Role: "unknown"},
		{Key: "unknown-permission", Permissions: []Permission{"unknown"}},
		{Key: "empty-permissions", Permissions: []Permission{}},
		{Key: "ambiguous", Role: RoleFull, Permissions: []Permission{PermissionRunsRead}},
		{Key: "", Role: RoleFull},
	}

	for _, testCase := range []struct {
		name        string
		key         string
		role        Role
		ok          bool
		allowRuns   bool
		allowPoll   bool
		allowDelete bool
	}{
		{"read", "reader", RoleRead, true, true, false, false},
		{"inspect", "inspector", RoleInspect, true, true, false, false},
		{"write", "writer", RoleWrite, true, true, true, false},
		{"full", "admin", RoleFull, true, true, true, true},
		{"custom", "poller", "", true, true, true, false},
		{"missing", "", "", false, false, false, false},
		{"incorrect", "other", "", false, false, false, false},
		{"invalid role", "invalid-role", "", false, false, false, false},
		{"unknown permission", "unknown-permission", "", false, false, false, false},
		{"empty permissions", "empty-permissions", "", false, false, false, false},
		{"ambiguous", "ambiguous", "", false, false, false, false},
		{"partial match", "read", "", false, false, false, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set(KeyHeader, testCase.key)

			grant, ok := keys.Authenticate(request)
			if grant.Role != testCase.role || ok != testCase.ok {
				t.Fatalf("Authenticate() = (role %q, %t), want (%q, %t)", grant.Role, ok, testCase.role, testCase.ok)
			}

			if grant.Allows(PermissionRunsRead) != testCase.allowRuns ||
				grant.Allows(PermissionPollTrigger) != testCase.allowPoll ||
				grant.Allows(PermissionProjectsDelete) != testCase.allowDelete {
				t.Fatal("authenticated grant has unexpected permissions")
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
		{name: "custom permissions", keys: Keyring{{Key: secret, Permissions: []Permission{PermissionPollTrigger, PermissionRunsRead}}}},
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
		{name: "role and permissions", keys: Keyring{{Key: secret, Role: RoleRead, Permissions: []Permission{PermissionJobsRead}}}, wantErr: "mutually exclusive"},
		{name: "role and empty permissions", keys: Keyring{{Key: secret, Role: RoleRead, Permissions: []Permission{}}}, wantErr: "mutually exclusive"},
		{name: "empty permissions", keys: Keyring{{Key: secret, Permissions: []Permission{}}}, wantErr: "non-empty"},
		{name: "unknown permission", keys: Keyring{{Key: secret, Permissions: []Permission{Permission(secret)}}}, wantErr: "unknown permission"},
		{name: "duplicate permission", keys: Keyring{{Key: secret, Permissions: []Permission{PermissionJobsRead, PermissionJobsRead}}}, wantErr: "duplicate permission"},
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

func TestGrantSignature(t *testing.T) {
	t.Parallel()

	read, err := (APIKey{Role: RoleRead}).Grant()
	if err != nil {
		t.Fatal(err)
	}

	custom, err := (APIKey{Permissions: []Permission{
		PermissionSyncWindowsRead, PermissionJobsRead, PermissionRunsRead, PermissionHealthRead,
	}}).Grant()
	if err != nil {
		t.Fatal(err)
	}

	if read.Signature() != custom.Signature() {
		t.Fatal("equivalent role and custom grants have different signatures")
	}

	narrow, err := (APIKey{Permissions: []Permission{PermissionRunsRead}}).Grant()
	if err != nil {
		t.Fatal(err)
	}

	if read.Signature() == narrow.Signature() {
		t.Fatal("different grants have the same signature")
	}
}
