package restapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"strings"

	"golang.org/x/net/http/httpguts"

	"github.com/kimdre/doco-cd/internal/common/types/set"
)

const KeyHeader = "x-api-key" // Header for API key

var (
	ErrInvalidApiKey     = errors.New("invalid api key")
	ErrInvalidAction     = errors.New("invalid action")
	ErrInvalidHTTPMethod = errors.New("invalid http method")
)

// Role identifies the operations an API key can perform.
type Role string

const (
	RoleRead Role = "read"
	// RoleInspect also allows project and stack details, which can contain secrets.
	RoleInspect Role = "inspect"
	RoleWrite   Role = "write"
	RoleFull    Role = "full"
)

// Permission identifies an API capability granted to a key.
type Permission string

const (
	PermissionHealthRead      Permission = "health:read"
	PermissionJobsRead        Permission = "jobs:read"
	PermissionJobsTrigger     Permission = "jobs:trigger"
	PermissionMirrorsCompact  Permission = "mirrors:compact"
	PermissionPollTrigger     Permission = "poll:trigger"
	PermissionProjectsControl Permission = "projects:control"
	PermissionProjectsDelete  Permission = "projects:delete"
	PermissionProjectsInspect Permission = "projects:inspect"
	PermissionRunsRead        Permission = "runs:read"
	PermissionStacksControl   Permission = "stacks:control"
	PermissionStacksDelete    Permission = "stacks:delete"
	PermissionStacksInspect   Permission = "stacks:inspect"
	PermissionSyncWindowsRead Permission = "sync_windows:read"
)

var rolePermissions = func() map[Role]set.Set[Permission] {
	read := set.New(
		PermissionHealthRead,
		PermissionJobsRead,
		PermissionRunsRead,
		PermissionSyncWindowsRead,
	)
	inspect := set.Union(read, set.New(
		PermissionProjectsInspect,
		PermissionStacksInspect,
	))
	write := set.Union(inspect, set.New(
		PermissionJobsTrigger,
		PermissionMirrorsCompact,
		PermissionPollTrigger,
		PermissionProjectsControl,
		PermissionStacksControl,
	))
	full := set.Union(write, set.New(
		PermissionProjectsDelete,
		PermissionStacksDelete,
	))

	return map[Role]set.Set[Permission]{
		RoleRead:    read,
		RoleInspect: inspect,
		RoleWrite:   write,
		RoleFull:    full,
	}
}()

// Allows reports whether the role grants the required permission.
func (role Role) Allows(required Permission) bool {
	return rolePermissions[role].Contains(required)
}

// Valid reports whether the role is a configured role preset.
func (role Role) Valid() bool {
	_, ok := rolePermissions[role]

	return ok
}

// MinimumRole returns the least-privileged preset that grants a permission.
func MinimumRole(permission Permission) (Role, bool) {
	for _, role := range []Role{RoleRead, RoleInspect, RoleWrite, RoleFull} {
		if role.Allows(permission) {
			return role, true
		}
	}

	return "", false
}

// Grant contains the permissions associated with an authenticated API key.
type Grant struct {
	Role        Role
	permissions set.Set[Permission]
}

// Allows reports whether the grant contains the required permission.
func (grant Grant) Allows(required Permission) bool {
	return grant.permissions.Contains(required)
}

// Signature identifies an effective permission set independently of its role or order.
func (grant Grant) Signature() string {
	permissions := set.SortedSlice(grant.permissions)
	parts := make([]string, len(permissions))

	for i, permission := range permissions {
		parts[i] = string(permission)
	}

	return strings.Join(parts, ",")
}

// APIKey binds a credential to a role or an explicit set of permissions.
type APIKey struct {
	Key         string       `yaml:"key" json:"-"`
	Role        Role         `yaml:"role,omitempty"`
	Permissions []Permission `yaml:"permissions,omitempty"`
}

// Grant resolves a key's role or explicit permissions without including its secret.
func (key APIKey) Grant() (Grant, error) {
	if key.Role != "" && key.Permissions != nil {
		return Grant{}, errors.New("role and permissions are mutually exclusive")
	}

	if key.Permissions == nil {
		if !key.Role.Valid() {
			return Grant{}, errors.New("role must be read, inspect, write, or full, or permissions must be specified")
		}

		return Grant{Role: key.Role, permissions: rolePermissions[key.Role]}, nil
	}

	if len(key.Permissions) == 0 {
		return Grant{}, errors.New("permissions must be a non-empty list")
	}

	permissions := set.New[Permission]()

	for i, permission := range key.Permissions {
		if _, ok := MinimumRole(permission); !ok {
			return Grant{}, fmt.Errorf("unknown permission at position %d", i+1)
		}

		if permissions.Contains(permission) {
			return Grant{}, fmt.Errorf("duplicate permission at position %d", i+1)
		}

		permissions.Add(permission)
	}

	return Grant{permissions: permissions}, nil
}

// Keyring contains the configured API credentials.
type Keyring []APIKey

// Validate rejects invalid and ambiguous credentials without exposing their values.
func (keys Keyring) Validate() error {
	seen := set.New[string]()

	for i, key := range keys {
		if strings.TrimSpace(key.Key) == "" {
			return fmt.Errorf("API_KEYS entry %d: key must not be empty", i+1)
		}

		if textproto.TrimString(key.Key) != key.Key || !httpguts.ValidHeaderFieldValue(key.Key) {
			return fmt.Errorf("API_KEYS entry %d: key must be a valid HTTP header value without leading or trailing spaces or tabs", i+1)
		}

		if _, err := key.Grant(); err != nil {
			return fmt.Errorf("API_KEYS entry %d: %w", i+1, err)
		}

		if seen.Contains(key.Key) {
			return fmt.Errorf("API_KEYS entry %d: duplicate key", i+1)
		}

		seen.Add(key.Key)
	}

	return nil
}

// Authenticate returns the grant associated with the request's API key.
func (keys Keyring) Authenticate(r *http.Request) (Grant, bool) {
	value := r.Header.Get(KeyHeader)
	if value == "" {
		return Grant{}, false
	}

	provided := sha256.Sum256([]byte(value))

	var matched *APIKey

	for i := range keys {
		key := &keys[i]

		expected := sha256.Sum256([]byte(key.Key))
		if subtle.ConstantTimeCompare(provided[:], expected[:]) == 1 && key.Key != "" {
			matched = key
		}
	}

	if matched == nil {
		return Grant{}, false
	}

	grant, err := matched.Grant()

	return grant, err == nil
}
