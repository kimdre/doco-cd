//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// notificationBodyTemplate renders the stack on the first line and one commit subject per
// following line, so a scenario can assert on the changelog without parsing prose.
const notificationBodyTemplate = "{{.Stack}}{{range .Commits}}\n{{.Subject}}{{end}}"

// Notification is one Apprise request the daemon sent, in the shape
// notificationBodyTemplate renders.
type Notification struct {
	Title   string
	Stack   string
	Commits []string
}

// CaptureNotifications points the daemon's Apprise client at the gitserver, which logs
// every request body. Call before Start.
func (h *Harness) CaptureNotifications() {
	h.t.Helper()

	h.SetEnv("APPRISE_API_URL", "http://gitserver/notify")
	h.SetEnv("APPRISE_NOTIFY_URLS", "json://localhost")
	h.SetEnv("APPRISE_NOTIFY_LEVEL", "success")
	h.SetEnv("APPRISE_NOTIFY_BODY_TEMPLATE", notificationBodyTemplate)
}

// Notifications returns every notification the daemon sent so far, oldest first.
func (h *Harness) Notifications() []Notification {
	h.t.Helper()

	var notifications []Notification

	for line := range strings.SplitSeq(h.containerLogs(h.gitSrv), "\n") {
		n, ok := parseNotification(strings.TrimSpace(line))
		if ok {
			notifications = append(notifications, n)
		}
	}

	return notifications
}

// parseNotification reads one line of the gitserver's notify log. nginx writes the request
// body JSON-escaped, so it is unquoted first. Anything else in the log is skipped.
func parseNotification(line string) (Notification, bool) {
	if !strings.HasPrefix(line, "{") {
		return Notification{}, false
	}

	var raw string
	if err := json.Unmarshal([]byte(`"`+line+`"`), &raw); err != nil {
		return Notification{}, false
	}

	var req struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		return Notification{}, false
	}

	lines := strings.Split(req.Body, "\n")

	return Notification{
		Title:   req.Title,
		Stack:   lines[0],
		Commits: lines[1:],
	}, true
}

// StackNotifications returns the notifications sent for stack, oldest first.
func (h *Harness) StackNotifications(stack string) []Notification {
	h.t.Helper()

	var matched []Notification

	for _, n := range h.Notifications() {
		if n.Stack == stack {
			matched = append(matched, n)
		}
	}

	return matched
}

// NextNotification waits for the next notification of stack that this method has not
// returned yet, so a scenario reads the deploys of a stack one by one.
func (h *Harness) NextNotification(stack string, timeout time.Duration) Notification {
	h.t.Helper()

	if h.notificationsSeen == nil {
		h.notificationsSeen = map[string]int{}
	}

	seen := h.notificationsSeen[stack]

	var next Notification

	h.WaitFor(timeout, fmt.Sprintf("notification #%d of stack %s", seen+1, stack), func() bool {
		matched := h.StackNotifications(stack)
		if len(matched) <= seen {
			return false
		}

		next = matched[seen]

		return true
	})

	h.notificationsSeen[stack] = seen + 1

	return next
}
