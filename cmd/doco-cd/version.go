package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/avast/retry-go/v5"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
)

type githubRelease struct {
	TagName      string `json:"tag_name"`
	IsPreRelease bool   `json:"prerelease"`
	IsDraft      bool   `json:"draft"`
}

// getLatestAppVersion gets the latest application version from the GitHub releases API.
func getLatestAppReleaseVersion() (string, error) {
	const releaseApiUrl = "https://api.github.com/repos/kimdre/doco-cd/releases"

	httpClient := &http.Client{Timeout: 3 * time.Second}

	return getLatestAppReleaseVersionFromURL(releaseApiUrl, httpClient)
}

func getLatestAppReleaseVersionFromURL(releaseApiURL string, httpClient *http.Client) (string, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	var (
		releases []githubRelease
		resp     *http.Response
	)

	err := retry.New(
		retry.Attempts(5),
		retry.Delay(250*time.Millisecond),
		retry.DelayType(retry.BackOffDelay),
	).Do(
		func() error {
			var err error

			resp, err = httpClient.Get(releaseApiURL)
			if err != nil {
				return err
			}

			defer func() {
				if resp.Body != nil {
					_ = resp.Body.Close()
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

func notificationForNewAppVersion(log *slog.Logger) {
	latestVersion, err := getLatestAppReleaseVersion()
	if err != nil {
		log.Error("failed to get latest application release version", logger.ErrAttr(err))
	} else {
		if config.AppVersion != latestVersion {
			log.Warn("new application version available",
				slog.String("current", config.AppVersion),
				slog.String("latest", latestVersion),
			)

			err = notification.Send(notification.Info,
				"New version of doco-cd is available",
				fmt.Sprintf("Current Version: %s\nLatest Version: %s\n\nhttps://github.com/kimdre/doco-cd/releases", config.AppVersion, latestVersion),
				notification.Metadata{})
			if err != nil {
				return
			}
		} else {
			log.Debug("application is up to date", slog.String("version", config.AppVersion))
		}
	}
}
