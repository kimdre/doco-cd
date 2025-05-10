package utils

import (
	"errors"
	"runtime/debug"
	"strings"
)

// GetModuleVersion retrieves the version of a specified module from the build info.
func GetModuleVersion(module string) (string, error) {
	var version string

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", errors.New("could not read build info")
	}

	for _, dep := range info.Deps {
		if dep.Path == module {
			version = dep.Version
			break
		}
	}

	if version == "" {
		return "", errors.New("module not found in build info")
	}

	version = strings.TrimPrefix(version, "v")

	return version, nil
}
