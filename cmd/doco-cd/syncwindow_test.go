package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/syncwindow"
)

func TestSyncWindowDeferredMessage(t *testing.T) {
	t.Parallel()

	blocked := &stages.SyncWindowBlockedError{Stacks: []string{"web"}, Windows: []string{"freeze"}}
	if got := syncWindowDeferredMessage(blocked); got != "deployment deferred by sync window" {
		t.Fatalf("message without next open = %q", got)
	}

	blocked.NextOpen = time.Date(2026, time.March, 10, 11, 0, 0, 0, time.UTC)
	if got := syncWindowDeferredMessage(blocked); got != "deployment deferred by sync window until 2026-03-10T11:00:00Z" {
		t.Fatalf("message = %q", got)
	}
}

func TestLogSyncWindows(t *testing.T) {
	t.Parallel()

	policy, err := syncwindow.Parse(`
- name: freeze
  kind: deny
  schedule: "0 9 * * *"
  duration: 2h
  timezone: UTC
  deployments: ["web"]
- name: business-hours
  kind: allow
  schedule: "0 8 * * 1-5"
  duration: 10h
  timezone: UTC
`)
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	var logs bytes.Buffer

	logSyncWindows(slog.New(slog.NewTextHandler(&logs, nil)), policy, time.Date(2026, time.March, 10, 10, 0, 0, 0, time.UTC))

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want 2:\n%s", len(lines), logs.String())
	}

	for _, want := range []string{"name=freeze", "kind=deny", "active=true", "active_until=2026-03-10T11:00:00.000Z", "deployments=[web]"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("first line %q does not contain %q", lines[0], want)
		}
	}

	if !strings.Contains(lines[1], "name=business-hours") || !strings.Contains(lines[1], "next_start=2026-03-11T08:00:00.000Z") {
		t.Errorf("second line %q does not describe the allow window", lines[1])
	}

	// Without windows nothing is logged.
	logs.Reset()
	logSyncWindows(slog.New(slog.NewTextHandler(&logs, nil)), nil, time.Now())

	if logs.Len() != 0 {
		t.Fatalf("logged %q without windows", logs.String())
	}
}
