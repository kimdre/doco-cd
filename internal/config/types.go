package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"gopkg.in/validator.v2"
)

type HttpUrl string // HttpUrl is a type for strings that represent HTTP URLs

// InitializeHttpUrlValidator registers a custom validation function for HTTP URLs.
func InitializeHttpUrlValidator() error {
	err := validator.SetValidationFunc("httpUrl", validateHttpUrl)
	if err != nil {
		return fmt.Errorf("error registering httpUrl validator: %w", err)
	}

	return nil
}

func init() {
	if err := InitializeHttpUrlValidator(); err != nil {
		// Log the error or handle it appropriately
		fmt.Println("Failed to initialize HttpUrl validator:", err)
	}
}

// validateHttpUrl checks if the given value is a valid HTTP or HTTPS URL.
func validateHttpUrl(v interface{}, param string) error {
	str, ok := v.(string)
	if !ok {
		if httpUrl, ok := v.(HttpUrl); ok {
			str = string(httpUrl)
		} else {
			return errors.New("validateHttpUrl: expected string or HttpUrl type")
		}
	}

	if str == "" {
		return nil // Empty string is considered valid
	}

	u, err := url.Parse(str)
	if err != nil {
		return fmt.Errorf("%w: failed to parse URL '%s'", ErrInvalidHttpUrl, str)
	}

	if !strings.HasPrefix(u.Scheme, "http") {
		return fmt.Errorf("%w: URL must start with http or https, got '%s'", ErrInvalidHttpUrl, str)
	}

	return nil
}
