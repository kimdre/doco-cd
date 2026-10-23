package bitwardensecretsmanager

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrNotSupported       = errors.New("bitwarden secrets manager is not supported in this build")
	ErrInvalidSecretPath  = errors.New("invalid secret path")
	ErrDuplicateSecretKey = errors.New("duplicate secret key")
)

// parseSecretPath parses a path with the format "organizationId[:projectId]".
func parseSecretPath(path string) (organizationID, projectID string, err error) {
	parts := strings.Split(strings.TrimSpace(path), ":")
	if len(parts) > 2 || parts[0] == "" {
		return "", "", fmt.Errorf("%w: expected format 'organizationId[:projectId]'", ErrInvalidSecretPath)
	}

	if len(parts) == 2 {
		projectID = parts[1]
	}

	return parts[0], projectID, nil
}
