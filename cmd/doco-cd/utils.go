package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/kimdre/doco-cd/internal/docker"
)

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

	lines := strings.Split(content, "\n")
	for _, line := range lines {
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
