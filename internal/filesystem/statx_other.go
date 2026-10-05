//go:build !linux

package filesystem

import (
	"os"
	"time"
)

// BirthTime returns the time path was created. It reports false if the file system does not record it.
func BirthTime(path string) (time.Time, bool, error) {
	if _, err := os.Stat(path); err != nil {
		return time.Time{}, false, err
	}

	return time.Time{}, false, nil
}

// Identity returns a string that identifies the file at path. It is empty on platforms that do not expose file
// identities, so every file at the same path is treated as the same file.
func Identity(path string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", err
	}

	return "", nil
}
