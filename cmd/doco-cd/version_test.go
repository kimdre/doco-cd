package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/notification"
)

func TestGetLatestAppReleaseVersion(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases" {
			t.Fatalf("expected request path /releases, got %q", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")

		_, err := w.Write([]byte(`[
			{"tag_name":"v9.9.9-rc1","prerelease":true,"draft":false},
			{"tag_name":"v9.9.9","prerelease":false,"draft":false},
			{"tag_name":"v9.9.8","prerelease":false,"draft":false}
		]`))
		if err != nil {
			t.Fatalf("failed to write test response: %v", err)
		}
	}))
	defer server.Close()

	version, err := getLatestAppReleaseVersionFromURL(server.URL+"/releases", server.Client())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if version != "v9.9.9" {
		t.Fatalf("expected latest stable version %q, got %q", "v9.9.9", version)
	}
}

func TestGetLatestAppReleaseVersion_StatusError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	defer server.Close()

	_, err := getLatestAppReleaseVersionFromURL(server.URL, server.Client())
	if err == nil {
		t.Fatal("expected an error, got nil")
	}

	if got := err.Error(); got == "" || !strings.Contains(got, "403 Forbidden") {
		t.Fatalf("expected 403 error, got %q", got)
	}
}

func TestGetLatestAppReleaseVersion_NoStableRelease(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		_, err := w.Write([]byte(`[
			{"tag_name":"v9.9.9-rc1","prerelease":true,"draft":false},
			{"tag_name":"v9.9.9-draft","prerelease":false,"draft":true}
		]`))
		if err != nil {
			t.Fatalf("failed to write test response: %v", err)
		}
	}))
	defer server.Close()

	_, err := getLatestAppReleaseVersionFromURL(server.URL, server.Client())
	if err == nil {
		t.Fatal("expected an error, got nil")
	}

	if got := err.Error(); got != "no stable release found" {
		t.Fatalf("expected %q, got %q", "no stable release found", got)
	}
}

type versionTestNotification struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Type  string `json:"type"`
}

func newVersionTestNotifier(t *testing.T, cfg notification.Config) (*notification.Notifier, <-chan versionTestNotification) {
	t.Helper()

	received := make(chan versionTestNotification, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST request, got %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)

			return
		}

		var payload versionTestNotification
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode notification: %v", err)
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		received <- payload

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	cfg.APIURL = server.URL

	notifier, err := notification.New(cfg)
	if err != nil {
		t.Fatalf("configure notifier: %v", err)
	}

	return notifier, received
}

func TestNotificationForNewAppVersion(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name              string
		enabled           bool
		version           string
		lookupErr         error
		wantLookups       int
		wantNotifications int
		wantLog           string
	}{
		{name: "disabled", version: app.Version + "-next"},
		{name: "disabled with lookup error", lookupErr: errors.New("lookup failed")},
		{
			name:              "new version",
			enabled:           true,
			version:           app.Version + "-next",
			wantLookups:       1,
			wantNotifications: 1,
			wantLog:           "new application version available",
		},
		{
			name:        "current version",
			enabled:     true,
			version:     app.Version,
			wantLookups: 1,
			wantLog:     "application is up to date",
		},
		{
			name:        "lookup failure",
			enabled:     true,
			lookupErr:   errors.New("lookup failed"),
			wantLookups: 1,
			wantLog:     "failed to get latest application release version",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			notifier, received := newVersionTestNotifier(t, notification.Config{
				NotifyURLs:   "apprise://test",
				NotifyLevel:  "info",
				BodyTemplate: "custom deployment body",
			})

			var logs bytes.Buffer

			log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			lookups := 0

			notificationForNewAppVersionWithLookup(log, notifier, tc.enabled, func() (string, error) {
				lookups++

				return tc.version, tc.lookupErr
			})

			if lookups != tc.wantLookups {
				t.Fatalf("release lookups = %d, want %d", lookups, tc.wantLookups)
			}

			if len(received) != tc.wantNotifications {
				t.Fatalf("notifications = %d, want %d", len(received), tc.wantNotifications)
			}

			if tc.wantLog == "" {
				if logs.Len() != 0 {
					t.Fatalf("unexpected release-check logs: %s", &logs)
				}
			} else if !strings.Contains(logs.String(), tc.wantLog) {
				t.Fatalf("logs = %q, want %q", logs.String(), tc.wantLog)
			}

			if tc.wantNotifications == 0 {
				return
			}

			payload := <-received
			if payload.Type != "info" || !strings.HasSuffix(payload.Title, "New version of doco-cd is available") {
				t.Fatalf("unexpected release notification: %+v", payload)
			}

			wantBody := fmt.Sprintf("Current Version: %s\nLatest Version: %s\n\nhttps://github.com/kimdre/doco-cd/releases", app.Version, tc.version)
			if payload.Body != wantBody {
				t.Fatalf("release body = %q, want %q", payload.Body, wantBody)
			}
		})
	}
}

func TestNotificationForNewAppVersion_DisabledPreservesDeploymentNotifications(t *testing.T) {
	t.Setenv("VERSION_CHECK_ENABLED", "false")
	t.Setenv("APPRISE_NOTIFY_LEVEL", "info")
	t.Setenv("WEBHOOK_SECRET", "secret")

	cfg, err := app.GetConfig()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	notifier, received := newVersionTestNotifier(t, notification.Config{
		NotifyURLs:  "apprise://test",
		NotifyLevel: cfg.AppriseNotifyLevel,
	})

	notificationForNewAppVersionWithLookup(slog.New(slog.DiscardHandler), notifier, cfg.VersionCheckEnabled, func() (string, error) {
		t.Fatal("disabled version check called the release lookup")

		return "", nil
	})

	if len(received) != 0 {
		t.Fatal("disabled version check sent a release notification")
	}

	for _, tc := range []struct {
		level notification.Level
		title string
		kind  string
	}{
		{level: notification.Info, title: "Deployment started", kind: "info"},
		{level: notification.Success, title: "Deployment completed", kind: "success"},
		{level: notification.Failure, title: "Deployment failed", kind: "failure"},
	} {
		if err := notifier.Send(tc.level, tc.title, "deployment message", notification.Metadata{Stack: "app"}); err != nil {
			t.Fatalf("send %q: %v", tc.title, err)
		}

		if len(received) != 1 {
			t.Fatalf("%q notifications = %d, want 1", tc.title, len(received))
		}

		payload := <-received
		if payload.Type != tc.kind || !strings.HasSuffix(payload.Title, tc.title) {
			t.Fatalf("unexpected deployment notification: %+v", payload)
		}
	}
}
