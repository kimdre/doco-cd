package onepassword

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

type OPSecretReference struct {
	Vault     string
	Item      string
	Section   string
	Field     string
	Attribute string
}

func ParseOPSecretReference(ref string) (*OPSecretReference, error) {
	parsed, err := url.Parse(ref)
	if err != nil {
		return nil, fmt.Errorf("invalid secret reference: %w", err)
	}

	if parsed.Scheme != "op" {
		return nil, fmt.Errorf("invalid secret reference scheme: %s", parsed.Scheme)
	}

	vault, err := url.PathUnescape(parsed.Host)
	if err != nil {
		return nil, fmt.Errorf("failed to decode vault segment: %w", err)
	}

	if strings.TrimSpace(vault) == "" {
		return nil, errors.New("invalid secret reference: vault segment is required")
	}

	segments, err := parseReferencePathSegments(parsed.Path)
	if err != nil {
		return nil, err
	}

	attribute := strings.TrimSpace(parsed.Query().Get("attribute"))
	if attribute != "" && attribute != "otp" {
		return nil, fmt.Errorf("unsupported secret reference attribute: %s", attribute)
	}

	out := &OPSecretReference{Vault: vault, Item: segments[0], Field: segments[len(segments)-1], Attribute: attribute}
	if len(segments) == 3 {
		out.Section = segments[1]
	}

	return out, nil
}

func parseReferencePathSegments(path string) ([]string, error) {
	trimmed := strings.Trim(path, "/")

	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 && len(parts) != 3 {
		return nil, errors.New("invalid secret reference path: expected item/field or item/section/field")
	}

	segments := make([]string, 0, len(parts))
	for _, part := range parts {
		decoded, err := url.PathUnescape(part)
		if err != nil {
			return nil, fmt.Errorf("failed to decode secret reference segment: %w", err)
		}

		if strings.TrimSpace(decoded) == "" {
			return nil, errors.New("invalid secret reference path: empty segment")
		}

		segments = append(segments, decoded)
	}

	return segments, nil
}
