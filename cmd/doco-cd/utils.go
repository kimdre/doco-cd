package main

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/kimdre/doco-cd/internal/docker"
)

// logRecoveredPanic logs a recovered panic value with a stack trace. It is
// meant to be called from a deferred closure that performs the recover(), so a
// panic in fire-and-forget (async) work cannot crash the entire process, since
// net/http's implicit recovery only covers the request goroutine, not
// goroutines spawned from a handler.
func logRecoveredPanic(log *slog.Logger, context string, r any) {
	log.Error("recovered from panic in background task",
		slog.String("context", context),
		slog.Any("panic", r),
		slog.String("stack", string(debug.Stack())),
	)
}

// getAppContainerID retrieves the application container ID from the cgroup mounts.
func getAppContainerID() (string, error) {
	const cgroupMounts = "/proc/self/mountinfo"

	data, err := os.ReadFile(cgroupMounts)
	if err != nil {
		return "", fmt.Errorf("failed to read %s: %w", cgroupMounts, err)
	}

	id := extractContainerIDFromMountInfo(string(data))
	if id != "" {
		return id, nil
	}

	return "", docker.ErrContainerIDNotFound
}

// extractContainerIDFromMountInfo extracts the container ID from the mount info content.
func extractContainerIDFromMountInfo(content string) string {
	containerIdPattern := regexp.MustCompile(`[a-z0-9]{64}`)

	lines := strings.SplitSeq(content, "\n")
	for line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		mountPath := fields[3]

		if strings.Contains(line, "/etc/hostname") {
			if matches := containerIdPattern.FindStringSubmatch(mountPath); len(matches) > 0 {
				return matches[0]
			}
		}
	}

	return ""
}
