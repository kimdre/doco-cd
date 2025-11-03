package notification

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
	appriseApiURL      = ""
	appriseNotifyUrls  = ""
	appriseNotifyLevel = Info
)

// appriseRequest represents the structure of a request to the Apprise notification service.
type appriseRequest struct {
	NotifyUrls string `json:"urls"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	Type       string `json:"type,omitempty"` // Optional field for specifying the type of notification (info, success, error, failure)
}

type Metadata struct {
	Repository string
	Stack      string
	Revision   string
	JobID      string
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
		return fmt.Errorf("failed to send request to Apprise: %w", err)
	}

	defer resp.Body.Close() // nolint:errcheck

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNoContent:
		return nil
	default:
		return fmt.Errorf("apprise request failed with status: %s", resp.Status)
	}
}

// SetAppriseConfig sets the configuration for the Apprise notification service.
func SetAppriseConfig(apiURL, notifyUrls, notifyLevel string) {
	appriseApiURL = apiURL
	appriseNotifyUrls = notifyUrls
	appriseNotifyLevel = parseLevel(notifyLevel)
}

// Send sends a notification using the Apprise service based on the provided configuration and parameters.
func Send(level level, title, message string, metadata Metadata) error {
	if appriseApiURL == "" || appriseNotifyUrls == "" {
		return nil
	}

	if level < appriseNotifyLevel {
		return nil // Do not send notification if the level is lower than the configured level
	}

	title = levelEmojis[level] + " " + title

	message = formatMessage(message, metadata)

	err := send(appriseApiURL, appriseNotifyUrls, title, message, logLevels[level])
	if err != nil {
		return fmt.Errorf("failed to send notification: %w", err)
	}

	return nil
}

// formatMessage formats the message by adding a newline after the first colon and appending the revision if provided.
func formatMessage(message string, m Metadata) string {
	message = strings.Replace(message, ": ", ":\n", 1)

	var metadataInfo string

	fields := []struct {
		key, value string
	}{
		{"repository", m.Repository},
		{"stack", m.Stack},
		{"revision", m.Revision},
		{"job_id", m.JobID},
	}

	var sb strings.Builder

	for _, f := range fields {
		if f.value != "" {
			_, _ = sb.WriteString(fmt.Sprintf("\n%s: %s", f.key, f.value))
		}
	}

	metadataInfo += sb.String()

	return fmt.Sprintf("%s\n%s", message, metadataInfo)
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
