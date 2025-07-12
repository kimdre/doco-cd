package config

import (
	"errors"
	"fmt"
	"net/url"

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

	u, err := url.Parse(str)
	if err != nil {
		return fmt.Errorf("%w: failed to parse URL '%s'", ErrInvalidHttpUrl, str)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: URL must start with http or https, got '%s'", ErrInvalidHttpUrl, str)
	}

	if u.Host == "" {
		return fmt.Errorf("%w: URL must have a host, got '%s'", ErrInvalidHttpUrl, str)
	}

	return nil
}
