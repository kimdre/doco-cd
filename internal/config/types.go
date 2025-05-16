package config

import (
	"errors"
	"net/url"
	"strings"

	"gopkg.in/validator.v2"
)

type HttpUrl string // HttpUrl is a type for strings that represent HTTP URLs

// Initialize the validator at package import
func init() {
	// Register the custom validation function for HttpUrl
	err := validator.SetValidationFunc("httpUrl", validateHttpUrl)
	if err != nil {
		panic("error registering httpUrl validator: " + err.Error())
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
		return errors.New("invalid url syntax: " + err.Error())
	}

	if !strings.HasPrefix(u.Scheme, "http") {
		return ErrInvalidUrl
	}

	return nil
}
