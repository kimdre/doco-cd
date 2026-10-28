package imagepull

import "strings"

// IsTransientError reports whether an image pull lost its lease or layer files.
// Concurrent image pruning can cause this on the containerd image store
// (moby/moby#53321). A new pull can recover.
func IsTransientError(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "lease does not exist") ||
		strings.Contains(msg, "failed commit on ref") ||
		(strings.Contains(msg, "failed to extract layer") && strings.Contains(msg, "no such file or directory"))
}
