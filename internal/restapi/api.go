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

// Allows reports whether the role permits the required operation.
func (role Role) Allows(required Role) bool {
	switch required {
	case RoleRead:
		return role == RoleRead || role == RoleInspect || role == RoleWrite || role == RoleFull
	case RoleInspect:
		return role == RoleInspect || role == RoleWrite || role == RoleFull
	case RoleWrite:
		return role == RoleWrite || role == RoleFull
	case RoleFull:
		return role == RoleFull
	default:
		return false
	}
}

// APIKey binds a credential to an access role.
type APIKey struct {
	Key  string `yaml:"key" json:"-"`
	Role Role   `yaml:"role"`
}

// Keyring contains the configured API credentials.
type Keyring []APIKey

// Validate rejects invalid and ambiguous credentials without exposing their values.
func (keys Keyring) Validate() error {
	seen := make(map[string]struct{}, len(keys))

	for i, key := range keys {
		if strings.TrimSpace(key.Key) == "" {
			return fmt.Errorf("API_KEYS entry %d: key must not be empty", i+1)
		}

		if textproto.TrimString(key.Key) != key.Key || !httpguts.ValidHeaderFieldValue(key.Key) {
			return fmt.Errorf("API_KEYS entry %d: key must be a valid HTTP header value without leading or trailing spaces or tabs", i+1)
		}

		if !key.Role.Allows(RoleRead) {
			return fmt.Errorf("API_KEYS entry %d: role must be read, inspect, write, or full", i+1)
		}

		if _, ok := seen[key.Key]; ok {
			return fmt.Errorf("API_KEYS entry %d: duplicate key", i+1)
		}

		seen[key.Key] = struct{}{}
	}

	return nil
}

// Authenticate returns the role associated with the request's API key.
func (keys Keyring) Authenticate(r *http.Request) (Role, bool) {
	value := r.Header.Get(KeyHeader)
	if value == "" {
		return "", false
	}

	provided := sha256.Sum256([]byte(value))

	var role Role

	for _, key := range keys {
		expected := sha256.Sum256([]byte(key.Key))
		if subtle.ConstantTimeCompare(provided[:], expected[:]) == 1 && key.Key != "" {
			role = key.Role
		}
	}

	return role, role.Allows(RoleRead)
}
