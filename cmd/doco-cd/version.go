package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// getLatestAppVersion gets the latest application version from the GitHub releases API.
func getLatestAppReleaseVersion() (string, error) {
	const releaseApiUrl = "https://api.github.com/repos/kimdre/doco-cd/releases"

	var (
		releases []struct {
			TagName      string `json:"tag_name"`
			IsPreRelease bool   `json:"prerelease"`
			IsDraft      bool   `json:"draft"`
		}
		resp *http.Response
		err  error
	)

	retries := 3
	httpClient := &http.Client{Timeout: 3 * time.Second}

	for i := 0; i < retries; i++ {
		resp, err = httpClient.Get(releaseApiUrl)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				break
			}

			_ = resp.Body.Close()

			return "", fmt.Errorf("failed to fetch releases: %s", resp.Status)
		}

		if resp != nil {
			_ = resp.Body.Close()
		}

		if i < retries-1 {
			time.Sleep(2 * time.Second)
		} else {
			return "", fmt.Errorf("failed to fetch releases after %d attempts: %w", retries, err)
		}
	}

	defer resp.Body.Close() // nolint:errcheck

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
