package imagepull

import "strings"

// IsTransientError reports whether the daemon lost an image pull's content lease.
// Concurrent image pruning can cause this on the containerd image store
// (moby/moby#53321). A new pull can recover.
func IsTransientError(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "lease does not exist") || strings.Contains(msg, "failed commit on ref")
}
