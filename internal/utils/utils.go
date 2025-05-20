package utils

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime/debug"
	"strings"
)

var (
	ErrModuleNotFound         = errors.New("module not found in build info")
	ErrBuildInfoUnavailable   = errors.New("build info unavailable")
	ErrInvalidFilePath        = errors.New("invalid file path")
	ErrPathOutsideTrustedRoot = errors.New("path is outside of trusted root")
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

// inTrustedRoot checks if the given path is within the trusted root directory.
func inTrustedRoot(path string, trustedRoot string) error {
	for path != "/" {
		path = filepath.Dir(path)
		if path == trustedRoot {
			return nil
		}
	}

	return ErrPathOutsideTrustedRoot
}

// VerifyAndSanitizePath checks if a file path is valid and sanitizes it.
func VerifyAndSanitizePath(path, trustedRoot string) (string, error) {
	c := filepath.Clean(path)

	err := inTrustedRoot(c, trustedRoot)
	if err != nil {
		return c, err
	} else {
		return c, nil
	}
}
