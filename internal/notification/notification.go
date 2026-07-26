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
	"text/template"
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
	appriseTemplate    *template.Template // optional user template rendering the notification body; nil = built-in format
)

// ErrNotifyFailed is returned when the Apprise request fails due to invalid notify URLs or unreachable service.
var ErrNotifyFailed = errors.New("request to apprise failed")

// ErrInvalidTemplate is returned when the configured notification body template fails to parse or execute.
var ErrInvalidTemplate = errors.New("invalid notification template")

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

// TemplateData is the data exposed to a user-configured notification body template.
// Metadata is embedded, so its fields (Repository, Stack, Revision, JobID, ...) are
// referenced directly, e.g. {{.Stack}} or {{.Repository}}.
type TemplateData struct {
	Level            string // notification level: info, success, warning or failure
	Emoji            string // level emoji (ℹ️/✅/⚠️/❌)
	Title            string // notification title, e.g. "Deployment completed"
	Message          string // core notification message
	IsReconciliation bool   // true when triggered by a reconciliation event
	Metadata                // embedded metadata fields
}

// parseTemplate parses and validates a notification body template. An empty
// string disables templating (nil result). The template is executed against a
// fully populated sample so field-reference mistakes fail fast at config time.
func parseTemplate(tmpl string) (*template.Template, error) {
	if strings.TrimSpace(tmpl) == "" {
		return nil, nil
	}

	t, err := template.New("notification").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidTemplate, err)
	}

	sample := TemplateData{
		Level: logLevels[Success], Emoji: levelEmojis[Success],
		Title: "Deployment completed", Message: "sample",
		Metadata: Metadata{
			Repository: "github.com/acme/app", Stack: "app",
			Revision: "refs/heads/main (abc123)", JobID: "sample",
		},
	}
	if err := t.Execute(io.Discard, sample); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidTemplate, err)
	}

	return t, nil
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
// bodyTemplate is an optional Go text/template rendering the notification body;
// an empty string keeps the built-in format. An invalid template is rejected.
func SetAppriseConfig(apiURL, notifyUrls, notifyLevel, bodyTemplate string) error {
	t, err := parseTemplate(bodyTemplate)
	if err != nil {
		return err
	}

	appriseConfigMu.Lock()
	defer appriseConfigMu.Unlock()

	appriseApiURL = apiURL
	appriseNotifyUrls = notifyUrls
	appriseNotifyLevel = parseLevel(notifyLevel)
	appriseTemplate = t

	return nil
}

func getAppriseConfig() (string, string, level, *template.Template) {
	appriseConfigMu.RLock()
	defer appriseConfigMu.RUnlock()

	return appriseApiURL, appriseNotifyUrls, appriseNotifyLevel, appriseTemplate
}

// Send sends a notification using the Apprise service based on the provided configuration and parameters.
func Send(level level, title, message string, metadata Metadata) error {
	apiURL, notifyURLs, notifyLevel, bodyTemplate := getAppriseConfig()

	if apiURL == "" || notifyURLs == "" {
		return nil
	}

	if level < notifyLevel {
		return nil // Do not send notification if the level is lower than the configured level
	}

	if bodyTemplate != nil {
		message = renderTemplate(bodyTemplate, level, title, message, metadata)
	} else {
		message = formatMessage(message, metadata)
	}

	title = formatTitle(level, title, metadata)

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

// renderTemplate renders the notification body using the configured template.
// On execution failure it falls back to the built-in format so an alert is never
// dropped because of a template mistake (config-time validation catches most).
func renderTemplate(t *template.Template, level level, title, message string, m Metadata) string {
	data := TemplateData{
		Level:            logLevels[level],
		Emoji:            levelEmojis[level],
		Title:            strings.TrimSpace(title),
		Message:          strings.TrimRight(message, "\n"),
		IsReconciliation: strings.TrimSpace(m.ReconciliationEvent) != "",
		Metadata:         m,
	}

	var sb strings.Builder
	if err := t.Execute(&sb, data); err != nil {
		return formatMessage(message, m)
	}

	return strings.TrimRight(sb.String(), "\n")
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
