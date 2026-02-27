package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/avast/retry-go/v5"
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
	)

	httpClient := &http.Client{Timeout: 3 * time.Second}

	err := retry.New(
		retry.Attempts(5),
		retry.Delay(250*time.Millisecond),
		retry.DelayType(retry.BackOffDelay),
	).Do(
		func() error {
			var err error

			resp, err = httpClient.Get(releaseApiUrl)
			if err != nil {
				return err
			}

			defer func() {
				if resp.Body != nil {
					resp.Body.Close()
				}
			}()

			if resp.StatusCode != http.StatusOK {
				return errors.New("unexpected status code: " + resp.Status)
			}

			return json.NewDecoder(resp.Body).Decode(&releases)
		},
	)
	if err != nil {
		return "", fmt.Errorf("failed to fetch releases: %w", err)
	}

	for _, release := range releases {
		if !release.IsPreRelease && !release.IsDraft {
			return release.TagName, nil
		}
	}

	return "", errors.New("no stable release found")
}
