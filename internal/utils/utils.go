package utils

import (
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
)

var (
	ErrModuleNotFound       = errors.New("module not found in build info")
	ErrBuildInfoUnavailable = errors.New("build info unavailable")
)

// GetModuleVersion retrieves the version of a specified module from the build info.
func GetModuleVersion(module string) (string, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", ErrBuildInfoUnavailable
	}

	for _, dep := range info.Deps {
		if dep.Path == module {
			return strings.TrimPrefix(dep.Version, "v"), nil
		}
	}

	return "", fmt.Errorf("%w: %s", ErrModuleNotFound, module)
}
