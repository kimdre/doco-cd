package notification

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

type level int

const (
	Info    level = iota // Informational messages
	Success              // Successful operations
	Warning              // Warning messages indicating potential issues
	Failure              // Error messages indicating failure of operations
)

var logLevels = map[level]string{
	Info:    "info",
	Success: "success",
	Warning: "warning",
	Failure: "failure",
}

var levelEmojis = map[level]string{
	Info:    "ℹ️",
	Success: "✅",
	Warning: "⚠️",
	Failure: "❌",
}

var (
	appriseConfigMu    sync.RWMutex
	appriseApiURL      = ""
	appriseNotifyUrls  = ""
	appriseNotifyLevel = Info
)

// ErrNotifyFailed is returned when the Apprise request fails due to invalid notify URLs or unreachable service.
var ErrNotifyFailed = errors.New("request to apprise failed")

// appriseRequest represents the structure of a request to the Apprise notification service.
type appriseRequest struct {
	NotifyUrls string `json:"urls"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	Type       string `json:"type,omitempty"` // Optional field for specifying the type of notification (info, success, error, failure)
}

type Metadata struct {
	Repository          string
	Stack               string
	Revision            string
	JobID               string
	TraceID             string
	ReconciliationEvent string
	AffectedActorKind   string
	AffectedActorID     string
	AffectedActorName   string
}

// parseLevel converts a string representation of a log level to the level type.
func parseLevel(level string) level {
	switch level {
	case logLevels[Info]:
		return Info
	case logLevels[Success]:
		return Success
	case logLevels[Warning]:
		return Warning
	case logLevels[Failure]:
		return Failure
	default:
		return Info // Default to Info if the level is not recognized
	}
}

// send a notification to the Apprise service.
func send(apiUrl, notifyUrls, title, message, level string) error {
	jsonData, err := json.Marshal(appriseRequest{
		NotifyUrls: notifyUrls,
		Title:      title,
		Body:       message,
		Type:       level,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal appriseRequest: %w", err)
	}

	resp, err := http.Post(apiUrl, "application/json", bytes.NewBuffer(jsonData)) // #nosec G107
	if err != nil {
		if strings.Contains(err.Error(), "malformed HTTP status code") {
			return ErrNotifyFailed
		}

		return fmt.Errorf("failed to send request to Apprise: %w", err)
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	// Drain the body so the underlying transport can safely reuse the connection.
	_, _ = io.Copy(io.Discard, resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNoContent:
		return nil
	case http.StatusFailedDependency:
		return ErrNotifyFailed
	default:
		return fmt.Errorf("apprise request failed with status: %s", resp.Status)
	}
}

// SetAppriseConfig sets the configuration for the Apprise notification service.
func SetAppriseConfig(apiURL, notifyUrls, notifyLevel string) {
	appriseConfigMu.Lock()
	defer appriseConfigMu.Unlock()

	appriseApiURL = apiURL
	appriseNotifyUrls = notifyUrls
	appriseNotifyLevel = parseLevel(notifyLevel)
}

func getAppriseConfig() (string, string, level) {
	appriseConfigMu.RLock()
	defer appriseConfigMu.RUnlock()

	return appriseApiURL, appriseNotifyUrls, appriseNotifyLevel
}

// Send sends a notification using the Apprise service based on the provided configuration and parameters.
func Send(level level, title, message string, metadata Metadata) error {
	apiURL, notifyURLs, notifyLevel := getAppriseConfig()

	if apiURL == "" || notifyURLs == "" {
		return nil
	}

	if level < notifyLevel {
		return nil // Do not send notification if the level is lower than the configured level
	}

	title = formatTitle(level, title, metadata)

	message = formatMessage(message, metadata)

	err := send(apiURL, notifyURLs, title, message, logLevels[level])
	if err != nil {
		return fmt.Errorf("failed to send notification: %w", err)
	}

	return nil
}

func formatTitle(level level, title string, metadata Metadata) string {
	formattedTitle := strings.TrimSpace(title)

	if strings.TrimSpace(metadata.ReconciliationEvent) != "" {
		formattedTitle = "[R] " + formattedTitle
	}

	return levelEmojis[level] + " " + formattedTitle
}

// formatMessage renders notifications as plain message text followed by structured metadata.
func formatMessage(message string, m Metadata) string {
	var sb strings.Builder
	trimmedMessage := strings.TrimRight(message, "\n")
	isReconciliation := strings.TrimSpace(m.ReconciliationEvent) != ""

	sb.WriteString(trimmedMessage)

	fields := map[string]string{}
	reconciliationFields := map[string]string{}

	if m.Repository != "" {
		fields["repository"] = m.Repository
	}

	if m.Stack != "" {
		fields["stack"] = m.Stack
	}

	if m.Revision != "" {
		fields["revision"] = m.Revision
	}

	if m.JobID != "" && !isReconciliation {
		fields["job_id"] = m.JobID
	}

	if m.ReconciliationEvent != "" {
		reconciliationFields["event"] = m.ReconciliationEvent
	}

	if m.TraceID != "" && isReconciliation {
		reconciliationFields["trace_id"] = m.TraceID
	}

	actorKind := strings.TrimSpace(strings.ToLower(m.AffectedActorKind))
	switch actorKind {
	case "container":
		if m.AffectedActorID != "" {
			reconciliationFields["container_id"] = m.AffectedActorID
		}

		if m.AffectedActorName != "" {
			reconciliationFields["container_name"] = m.AffectedActorName
		}
	case "service":
		if m.AffectedActorID != "" {
			reconciliationFields["service_id"] = m.AffectedActorID
		}

		if m.AffectedActorName != "" {
			reconciliationFields["service_name"] = m.AffectedActorName
		}
	}

	if len(fields) == 0 && len(reconciliationFields) == 0 {
		return sb.String()
	}

	if trimmedMessage != "" {
		sb.WriteString("\n\n")
	}

	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for idx, key := range keys {
		if idx > 0 {
			sb.WriteString("\n")
		}

		_, _ = fmt.Fprintf(&sb, "%s: %s", key, fields[key])
	}

	if len(reconciliationFields) > 0 {
		if len(keys) > 0 {
			sb.WriteString("\n")
		}

		sb.WriteString("reconciliation:")

		reconciliationKeys := make([]string, 0, len(reconciliationFields))
		for key := range reconciliationFields {
			reconciliationKeys = append(reconciliationKeys, key)
		}
		sort.Strings(reconciliationKeys)

		for _, key := range reconciliationKeys {
			_, _ = fmt.Fprintf(&sb, "\n  %s: %s", key, reconciliationFields[key])
		}
	}

	return sb.String()
}

func GetRevision(reference, commitSHA string) string {
	if reference == "" && commitSHA == "" {
		return ""
	}

	switch "" {
	case reference:
		return commitSHA
	case commitSHA:
		return reference
	default:
		return fmt.Sprintf("%s (%s)", reference, commitSHA)
	}
}
