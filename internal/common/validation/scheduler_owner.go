package validation

import (
	"errors"
	"fmt"
)

// ValidateSchedulerOwnerID checks an instance ID or job owner label.
func ValidateSchedulerOwnerID(value string) error {
	if value == "" {
		return errors.New("owner ID must not be empty")
	}

	if len(value) > 63 {
		return errors.New("owner ID must be at most 63 characters")
	}

	for _, ch := range value {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' ||
			ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == '-' {
			continue
		}

		return fmt.Errorf("owner ID contains invalid character %q (allowed: letters, digits, '.', '_', '-')", ch)
	}

	return nil
}
