package notification

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/kimdre/doco-cd/internal/git"
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
	appriseTemplate    *template.Template // template rendering the notification body; defaults to defaultTemplate
)

const (
	maxAppriseErrorResponseBodyBytes = 4 * 1024
	redactedURL                      = "[REDACTED_URL]"
	redactedValue                    = "[REDACTED]"
)

var urlLikePattern = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s,"]+`)

// defaultTemplate reproduces the built-in notification body (message followed by
// sorted metadata). It is used whenever no APPRISE_NOTIFY_BODY_TEMPLATE is configured.
// The heavy lifting lives in TemplateData.DefaultBody, which is also exposed to
// user templates so they can extend the built-in format instead of replacing it.
var defaultTemplate = template.Must(template.New("notification").Parse("{{ .DefaultBody }}"))

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
	Context             string // Docker context the stack is deployed to (empty = default context)
	Target              string // Custom webhook/poll target suffix (e.g., "prod-vm" for .doco-cd.prod-vm.yml)
	Revision            string
	JobID               string
	TraceID             string
	ReconciliationEvent string
	AffectedActorKind   string
	AffectedActorID     string
	AffectedActorName   string
	Commits             []git.CommitInfo // commits deployed since the last deploy; empty on first deploy/failure/OCI
	Duration            time.Duration    // time from job start to the notification; zero when no deploy/destroy ran
	ChangedServices     []string         // services force-recreated by this deploy; empty when the whole stack is (re)deployed
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

// validateTemplate parses and validates a notification body template. An empty
// string returns a nil template, signalling the caller to use defaultTemplate.
// The template is executed against a fully populated sample so field-reference
// mistakes fail fast at config time.
func validateTemplate(tmpl string) (*template.Template, error) {
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
			Repository: "github.com/acme/app", Stack: "app", Context: "default",
			Revision: "refs/heads/main (abc123)", JobID: "sample",
			Commits: []git.CommitInfo{
				{Hash: "abc123", ShortHash: "abc123", Subject: "sample commit", Author: "Jane Doe"},
			},
			Duration:        42 * time.Second,
			ChangedServices: []string{"app"},
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
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNoContent:
		return nil
	case http.StatusFailedDependency:
		return fmt.Errorf("%w: apprise request failed with status: %s%s", ErrNotifyFailed, resp.Status, appriseResponseErrorDetails(resp))
	default:
		return fmt.Errorf("apprise request failed with status: %s%s", resp.Status, appriseResponseErrorDetails(resp))
	}
}

// appriseResponseErrorDetails reads the response body of an Apprise request and extracts error details, redacting sensitive information.
// It returns a string containing the error message and the redacted response body, or an empty string if no details are available.
func appriseResponseErrorDetails(resp *http.Response) string {
	if resp.Body == nil {
		return ""
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAppriseErrorResponseBodyBytes+1))
	if err != nil {
		return fmt.Sprintf(", failed to read response body: %v", err)
	}

	truncated := len(raw) > maxAppriseErrorResponseBodyBytes
	if truncated {
		raw = raw[:maxAppriseErrorResponseBodyBytes]
	}

	trimmedRaw := strings.TrimSpace(string(raw))
	if trimmedRaw == "" {
		return ""
	}

	parsedMessage, redactedBody := appriseErrorMessageAndBody(trimmedRaw)
	if truncated {
		redactedBody += " (truncated)"
	}

	if parsedMessage == "" {
		return ", response body: " + redactedBody
	}

	return fmt.Sprintf(", error: %s, response body: %s", parsedMessage, redactedBody)
}

// appriseErrorMessageAndBody takes a raw JSON string from an Apprise response, attempts to parse it,
// and extracts a preferred error message while redacting sensitive information.
// It returns the extracted error message and the redacted JSON body as strings.
// If parsing fails, it returns an empty error message and the redacted raw string.
func appriseErrorMessageAndBody(raw string) (string, string) {
	var payload any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return "", redactSensitiveText(raw)
	}

	redacted := redactJSONValue(payload, "")

	bodyBytes, err := json.Marshal(redacted)
	if err != nil {
		return "", redactSensitiveText(raw)
	}

	errorMessage := strings.TrimSpace(extractAppriseErrorMessage(redacted))
	if errorMessage != "" {
		errorMessage = redactSensitiveText(errorMessage)
	}

	return errorMessage, string(bodyBytes)
}

func extractAppriseErrorMessage(v any) string {
	msg := extractPreferredErrorMessage(v)
	if msg == "" {
		return ""
	}

	return strings.Join(strings.Fields(msg), " ")
}

// extractPreferredErrorMessage recursively traverses a JSON-like structure (maps and slices) to find the first
// non-empty string value associated with preferred keys such as "error", "message", "detail", "description", or "reason".
// It returns the first found message or an empty string if none are found.
func extractPreferredErrorMessage(v any) string {
	switch typed := v.(type) {
	case map[string]any:
		preferred := []string{"error", "message", "detail", "description", "reason"}
		for _, key := range preferred {
			val, ok := typed[key]
			if !ok {
				continue
			}

			if s := extractStringOrNestedMessage(val); s != "" {
				return s
			}
		}

		for _, val := range typed {
			if s := extractPreferredErrorMessage(val); s != "" {
				return s
			}
		}
	case []any:
		for _, item := range typed {
			if s := extractPreferredErrorMessage(item); s != "" {
				return s
			}
		}
	}

	return ""
}

// extractStringOrNestedMessage takes a value of any type and attempts to extract a string message from it.
// If the value is a string, it trims whitespace and returns it. If it's a map or slice, it recursively calls
// extractPreferredErrorMessage to find a nested message. For other types, it converts the value to a string.
func extractStringOrNestedMessage(v any) string {
	switch typed := v.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any, []any:
		return extractPreferredErrorMessage(typed)
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", typed))
	}
}

// redactJSONValue recursively traverses a JSON-like structure (maps and slices) and redacts sensitive information based on key names.
// It returns a new structure with sensitive values replaced by redactedValue or redactedURL for URL-like strings.
func redactJSONValue(v any, key string) any {
	switch typed := v.(type) {
	case map[string]any:
		redacted := make(map[string]any, len(typed))
		for k, val := range typed {
			redacted[k] = redactJSONValue(val, k)
		}

		return redacted
	case []any:
		redacted := make([]any, len(typed))
		for i := range typed {
			redacted[i] = redactJSONValue(typed[i], key)
		}

		return redacted
	case string:
		if isSensitiveJSONKey(key) {
			return redactedValue
		}

		return redactSensitiveText(typed)
	default:
		return typed
	}
}

// isSensitiveJSONKey checks if a given JSON key is considered sensitive based on predefined keywords.
// It returns true if the key contains any of the sensitive keywords, ignoring case and whitespace.
func isSensitiveJSONKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	if k == "" {
		return false
	}

	sensitiveKeys := []string{
		"url", "urls", "uri", "token", "secret", "password", "pass", "credential",
		"webhook", "target", "targets", "authorization", "auth", "api_key", "apikey",
	}
	for _, sensitive := range sensitiveKeys {
		if strings.Contains(k, sensitive) {
			return true
		}
	}

	return false
}

// redactSensitiveText takes a string and redacts any URL-like patterns, replacing them with redactedURL.
// It also trims whitespace and normalizes spaces to a single space.
func redactSensitiveText(s string) string {
	redacted := urlLikePattern.ReplaceAllString(s, redactedURL)
	redacted = strings.TrimSpace(redacted)

	return strings.Join(strings.Fields(redacted), " ")
}

// SetAppriseConfig sets the configuration for the Apprise notification service.
// bodyTemplate is an optional Go text/template rendering the notification body;
// an empty string keeps the built-in format (defaultTemplate). An invalid
// template is rejected.
func SetAppriseConfig(apiURL, notifyUrls, notifyLevel, bodyTemplate string) error {
	t, err := validateTemplate(bodyTemplate)
	if err != nil {
		return err
	}

	if t == nil {
		t = defaultTemplate
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

// SendOption customizes how a notification is rendered/sent.
type SendOption func(*sendOptions)

type sendOptions struct {
	skipBodyTemplate bool
}

// WithoutBodyTemplate renders the notification with the built-in body format,
// ignoring any configured APPRISE_NOTIFY_BODY_TEMPLATE. Use it for app-level
// notifications (e.g. the "new version available" ping) that are not tied to a
// deployment and therefore carry no stack/context/revision the template expects.
func WithoutBodyTemplate() SendOption {
	return func(o *sendOptions) {
		o.skipBodyTemplate = true
	}
}

// Send sends a notification using the Apprise service based on the provided configuration and parameters.
func Send(level level, title, message string, metadata Metadata, opts ...SendOption) error {
	apiURL, notifyURLs, notifyLevel, bodyTemplate := getAppriseConfig()

	if apiURL == "" || notifyURLs == "" {
		return nil
	}

	if level < notifyLevel {
		return nil // Do not send notification if the level is lower than the configured level
	}

	var o sendOptions
	for _, opt := range opts {
		opt(&o)
	}

	if o.skipBodyTemplate {
		bodyTemplate = nil // renderTemplate falls back to the built-in default body
	}

	message = renderTemplate(bodyTemplate, level, title, message, metadata)

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

// DefaultBody renders the built-in notification body: the message text followed
// by structured metadata. It backs defaultTemplate and is exposed to user
// templates as {{ .DefaultBody }}.
func (d TemplateData) DefaultBody() string {
	var sb strings.Builder

	m := d.Metadata
	trimmedMessage := strings.TrimRight(d.Message, "\n")
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

	if m.Duration > 0 {
		fields["duration"] = m.Duration.String()
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

// renderTemplate renders the notification body using the configured template
// (defaultTemplate when t is nil). On execution failure it falls back to the
// built-in body so an alert is never dropped because of a template mistake
// (config-time validation catches most).
func renderTemplate(t *template.Template, level level, title, message string, m Metadata) string {
	if t == nil {
		t = defaultTemplate
	}

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
		return data.DefaultBody()
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
