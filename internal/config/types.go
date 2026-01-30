package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"

	"gopkg.in/validator.v2"
)

type HttpUrl string // HttpUrl is a type for strings that represent HTTP URLs

// init is called when the package is initialized.
func init() {
	// Register the custom validation function for HttpUrl
	err := validator.SetValidationFunc("httpUrl", validateHttpUrl)
	if err != nil {
		panic("error registering httpUrl validator: " + err.Error())
	}
}

// validateHttpUrl checks if the given value is a valid HTTP or HTTPS URL.
func validateHttpUrl(v interface{}, _ string) error {
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

	// Accept classic SSH clone (e.g., git@github.com:owner/repo.git)
	sshClonePattern := regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:.+`)
	if sshClonePattern.MatchString(str) {
		return nil
	}

	u, err := url.Parse(str)
	if err != nil {
		return fmt.Errorf("%w: failed to parse URL '%s'", ErrInvalidHttpUrl, str)
	}

	// Accept http(s) and ssh scheme URLs
	switch u.Scheme {
	case "http", "https":
		if u.Host == "" {
			return fmt.Errorf("%w: URL must have a host, got '%s'", ErrInvalidHttpUrl, str)
		}

		return nil
	case "ssh":
		// ssh://host/owner/repo.git
		if u.Host == "" || u.Path == "" {
			return fmt.Errorf("%w: SSH URL must include host and path, got '%s'", ErrInvalidHttpUrl, str)
		}

		return nil
	default:
		return fmt.Errorf("%w: URL must start with http, https or ssh, got '%s'", ErrInvalidHttpUrl, str)
	}
}
