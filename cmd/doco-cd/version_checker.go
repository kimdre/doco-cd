package main

import (
	"encoding/json"
	"fmt"
	"github.com/kimdre/doco-cd/internal/logger"
	"net/http"
	"strings"
)

// getLatestAppVersion gets the latest application version from the GitHub releases API.
func getLatestAppReleaseVersion() (string, error) {
	const releaseApiUrl = "https://api.github.com/repos/kimdre/doco-cd/releases"

	resp, err := http.Get(releaseApiUrl)
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to fetch releases: %s", resp.Status)
	}

	var releases []struct {
		TagName      string `json:"tag_name"`
		IsPreRelease bool   `json:"prerelease"`
	}

	if err = json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return "", err
	}

	for _, release := range releases {
		if !release.IsPreRelease && strings.HasPrefix(release.TagName, "v") {
			return strings.TrimPrefix(release.TagName, "v"), nil
		}
	}

	return "", fmt.Errorf("no stable release found")
}

func logAppVersion(currentVersion string, log *logger.Logger) error {
	latestVersion, err := getLatestAppReleaseVersion()
	if err != nil {
		return fmt.Errorf("failed to get latest version: %w", err)
	}

	if currentVersion == latestVersion {
		log.Debug("Application is up to date", "version", currentVersion)
	} else {
		log.Warn("Application version mismatch", "current", currentVersion, "latest", latestVersion)
	}

	return nil
}
