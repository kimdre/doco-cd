package secrettypes

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/template"
)

// ErrBulkNotSupported is returned when a secret provider cannot fetch all secrets below a path.
var ErrBulkNotSupported = errors.New("fetching secrets by path is not supported by this secret provider")

// ExternalSecretsPath describes a provider path from which all secrets are fetched in bulk.
// Each secret becomes one interpolation variable named after the secret.
type ExternalSecretsPath struct {
	// Path is the provider specific location to read from. The format is the same as
	// a secret reference of the provider, but without the final key or secret part.
	Path string `yaml:"path" json:"path"`
	// Prefix is added to every variable name created from this path.
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	// Recursive also reads secrets in nested paths, if the provider supports it.
	Recursive bool `yaml:"recursive,omitempty" json:"recursive,omitempty"`
}

// Validate checks that the path entry is usable.
func (p ExternalSecretsPath) Validate() error {
	if strings.TrimSpace(p.Path) == "" {
		return errors.New("path must not be empty")
	}

	return nil
}

// NormalizeEnvVarName converts a secret name into a valid environment variable name.
// The prefix is added in front of the name. All letters become upper case and every character
// that is not a letter or digit becomes an underscore. A leading digit gets an underscore in front.
// An empty result is returned when the name does not contain any letter or digit.
func NormalizeEnvVarName(prefix, name string) string {
	var (
		b      strings.Builder
		hasAny bool
	)

	for _, r := range prefix + name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - ('a' - 'A'))

			hasAny = true
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)

			hasAny = true
		default:
			b.WriteByte('_')
		}
	}

	if !hasAny {
		return ""
	}

	out := b.String()
	if out[0] >= '0' && out[0] <= '9' {
		out = "_" + out
	}

	return out
}

// MergeBulkSecrets normalizes the names of the secrets found by the provider
// and adds them to dst. It returns an error when two secrets get the same variable name.
func MergeBulkSecrets(dst map[string]string, source ExternalSecretsPath, found map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(found)) {
		value := found[name]

		envName := NormalizeEnvVarName(source.Prefix, name)
		if envName == "" {
			return fmt.Errorf("secret name %q in path %q cannot be converted to a variable name", name, source.Path)
		}

		if _, exists := dst[envName]; exists {
			return fmt.Errorf("secret name %q in path %q creates the duplicate variable %q", name, source.Path, envName)
		}

		dst[envName] = value
	}

	return nil
}

// InterpolateExternalSecretsPaths expands Compose-style variables in the path of each entry
// when enabled. The input slice is never mutated.
func InterpolateExternalSecretsPaths(in []ExternalSecretsPath, enabled bool) ([]ExternalSecretsPath, error) {
	if !enabled {
		return in, nil
	}

	out := make([]ExternalSecretsPath, len(in))

	for i, entry := range in {
		value, err := template.SubstituteWithOptions(requireExternalSecretVariables(entry.Path), os.LookupEnv, template.WithoutLogging)
		if err != nil {
			return nil, fmt.Errorf("interpolate external secrets path %q: %w", entry.Path, err)
		}

		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("interpolate external secrets path %q: path is empty after interpolation", entry.Path)
		}

		entry.Path = value
		out[i] = entry
	}

	return out, nil
}
