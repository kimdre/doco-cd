package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// getLatestAppVersion gets the latest application version from the GitHub releases API.
func getLatestAppReleaseVersion() (string, error) {
	const releaseApiUrl = "https://api.github.com/repos/kimdre/doco-cd/releases"

	resp, err := http.Get(releaseApiUrl)
	if err != nil {
		return "", err
	}

	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to fetch releases: %s", resp.Status)
	}

	var releases []struct {
		TagName      string `json:"tag_name"`
		IsPreRelease bool   `json:"prerelease"`
		IsDraft      bool   `json:"draft"`
	}

	if err = json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return "", err
	}

	for _, release := range releases {
		if !release.IsPreRelease && !release.IsDraft {
			return release.TagName, nil
		}
	}

	return "", errors.New("no stable release found")
}
