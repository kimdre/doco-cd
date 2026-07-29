package module

import (
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
)

var (
	ErrBuildInfoUnavailable = errors.New("build info unavailable")
	ErrNotFound             = errors.New("module not found in build info")
)

// GetVersion retrieves the version of a specified module from the build info.
func GetVersion(module string) (string, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", ErrBuildInfoUnavailable
	}

	for _, dep := range info.Deps {
		if dep.Path == module {
			return strings.TrimPrefix(dep.Version, "v"), nil
		}
	}

	return "", fmt.Errorf("%w: %s", ErrNotFound, module)
}
