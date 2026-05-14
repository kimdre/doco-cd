package config

import (
	"fmt"
	"strings"
)

type SourceType string

const (
	SourceTypeGit SourceType = "git"
	SourceTypeOCI SourceType = "oci"

	// OciArtifactLayoutV1 is the strict, versioned OCI artifact layout currently supported by doco-cd.
	OciArtifactLayoutV1 = "doco.v1"
)

// NormalizeSourceType returns a canonical source type and defaults empty values to git.
func NormalizeSourceType(source SourceType) SourceType {
	s := SourceType(strings.ToLower(strings.TrimSpace(string(source))))
	if s == "" {
		return SourceTypeGit
	}

	return s
}

// ValidateSourceType validates source values used by configuration structs.
func ValidateSourceType(source SourceType) error {
	s := NormalizeSourceType(source)

	switch s {
	case SourceTypeGit, SourceTypeOCI:
		return nil
	default:
		return fmt.Errorf("invalid source type %q: must be %q or %q", s, SourceTypeGit, SourceTypeOCI)
	}
}
