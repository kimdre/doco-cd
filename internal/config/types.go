package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"gopkg.in/validator.v2"
)

// UrlValidator is the common interface that both GitUrl and OciUrl implement.
type UrlValidator interface {
	Validate() error
}

// HttpUrl is a type for strings that represent plain HTTP or HTTPS URLs (e.g. for API endpoints).
type HttpUrl string

// GitUrl is a type for strings that represent Git repository URLs (http, https, ssh, or scp-style).
type GitUrl string

// OciUrl is a type for strings that represent OCI artifact references (e.g. ghcr.io/org/repo:tag).
type OciUrl string

// init registers custom validator functions for HttpUrl, GitUrl and OciUrl.
func init() {
	if err := validator.SetValidationFunc("httpUrl", validateHttpUrl); err != nil {
		panic("error registering httpUrl validator: " + err.Error())
	}

	if err := validator.SetValidationFunc("gitUrl", validateGitUrl); err != nil {
		panic("error registering gitUrl validator: " + err.Error())
	}

	if err := validator.SetValidationFunc("ociUrl", validateOciUrl); err != nil {
		panic("error registering ociUrl validator: " + err.Error())
	}
}

// Validate checks whether the HttpUrl is a valid HTTP or HTTPS URL.
func (h HttpUrl) Validate() error {
	s := strings.TrimSpace(string(h))

	if s == "" {
		return nil // empty is handled by required checks
	}

	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("%w: failed to parse URL '%s'", ErrInvalidHttpUrl, s)
	}

	switch u.Scheme {
	case "http", "https":
		if u.Host == "" {
			return fmt.Errorf("%w: URL must have a host, got '%s'", ErrInvalidHttpUrl, s)
		}

		return nil
	default:
		return fmt.Errorf("%w: URL must start with http or https, got '%s'", ErrInvalidHttpUrl, s)
	}
}

// Validate checks whether the GitUrl is a valid Git repository URL.
func (g GitUrl) Validate() error {
	s := strings.TrimSpace(string(g))

	if s == "" {
		return nil // empty is handled by required checks
	}

	// Accept classic SSH clone syntax, e.g. git@github.com:owner/repo.git
	sshClonePattern := regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:.+`)
	if sshClonePattern.MatchString(s) {
		return nil
	}

	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("%w: failed to parse URL '%s'", ErrInvalidGitUrl, s)
	}

	switch u.Scheme {
	case "http", "https":
		if u.Host == "" {
			return fmt.Errorf("%w: URL must have a host, got '%s'", ErrInvalidGitUrl, s)
		}

		return nil
	case "ssh":
		if u.Host == "" || u.Path == "" {
			return fmt.Errorf("%w: SSH URL must include host and path, got '%s'", ErrInvalidGitUrl, s)
		}

		return nil
	default:
		return fmt.Errorf("%w: URL must start with http, https or ssh, got '%s'", ErrInvalidGitUrl, s)
	}
}

// Validate checks whether the OciUrl is a valid OCI artifact reference.
func (o OciUrl) Validate() error {
	s := strings.TrimSpace(string(o))

	if s == "" {
		return nil // empty is handled by required checks
	}

	if _, err := name.ParseReference(s, name.WeakValidation); err != nil {
		return fmt.Errorf("%w: '%s': %v", ErrInvalidOciUrl, s, err)
	}

	return nil
}

// Tag returns the tag portion of the OCI artifact reference (e.g. "main" from "ghcr.io/org/repo:main").
// Returns an empty string if the reference cannot be parsed.
func (o OciUrl) Tag() string {
	ref, err := name.ParseReference(strings.TrimSpace(string(o)), name.WeakValidation)
	if err != nil {
		return ""
	}

	return ref.Identifier()
}

// validateHttpUrl is the validator.v2 adapter for HttpUrl.
func validateHttpUrl(v any, _ string) error {
	switch t := v.(type) {
	case string:
		return HttpUrl(t).Validate()
	case HttpUrl:
		return t.Validate()
	default:
		return fmt.Errorf("validateHttpUrl: expected string or HttpUrl, got %T", v)
	}
}

// validateGitUrl is the validator.v2 adapter for GitUrl.
func validateGitUrl(v any, _ string) error {
	switch t := v.(type) {
	case string:
		return GitUrl(t).Validate()
	case GitUrl:
		return t.Validate()
	default:
		return fmt.Errorf("validateGitUrl: expected string or GitUrl, got %T", v)
	}
}

// validateOciUrl is the validator.v2 adapter for OciUrl.
func validateOciUrl(v any, _ string) error {
	switch t := v.(type) {
	case string:
		return OciUrl(t).Validate()
	case OciUrl:
		return t.Validate()
	default:
		return fmt.Errorf("validateOciUrl: expected string or OciUrl, got %T", v)
	}
}
