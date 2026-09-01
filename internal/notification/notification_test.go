package notification

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/common/id"
)

func TestSend(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		appriseURL  string
		expectedErr error
	}{
		{
			name:       "Valid Service URL",
			appriseURL: "apprise://example.test",
			// nil means success is expected
			expectedErr: nil,
		},
		{
			name:        "Invalid Service URL",
			appriseURL:  "pover://wrong@test",
			expectedErr: ErrNotifyFailed,
		},
	}

	metadata := Metadata{
		Repository: "test",
		Stack:      "test-stack",
		Revision:   "main",
		JobID:      id.New(),
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST request, got %s", r.Method)
		}

		var payload appriseRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("failed to decode apprise request: %v", err)
		}

		if !strings.HasPrefix(payload.NotifyUrls, "apprise://") {
			w.WriteHeader(http.StatusFailedDependency)
			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			notifier, err := New(Config{
				APIURL:                server.URL,
				NotifyURLs:            tc.appriseURL,
				NotifyLevel:           "info",
				FailureRepeatInterval: DefaultFailureRepeatInterval,
			})
			if err != nil {
				t.Fatalf("failed to create notifier: %v", err)
			}

			err = notifier.Send(Info, "Test Notification", "This is a test message", metadata)
			if tc.expectedErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}

				return
			}

			if !errors.Is(err, tc.expectedErr) {
				t.Fatalf("expected error wrapping %q, got: %v", tc.expectedErr, err)
			}
		})
	}
}

func TestNewValidatesBodyTemplate(t *testing.T) {
	t.Parallel()

	if _, err := New(Config{BodyTemplate: "{{.Missing}}"}); !errors.Is(err, ErrInvalidTemplate) {
		t.Fatalf("New() error = %v, want ErrInvalidTemplate", err)
	}
}

func TestNotifierFailureStateIsIsolated(t *testing.T) {
	t.Parallel()

	first, err := New(Config{FailureRepeatInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	second, err := New(Config{FailureRepeatInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	key := failureKey(Metadata{Repository: "acme/repo", Stack: "app"})
	fingerprint := failureFingerprint("Deployment Failed", "pull access denied")

	if !first.shouldSendFailure(key, fingerprint, now) {
		t.Fatal("first notifier should send its initial failure")
	}

	if first.shouldSendFailure(key, fingerprint, now.Add(time.Second)) {
		t.Fatal("first notifier should suppress its unchanged failure")
	}

	if !second.shouldSendFailure(key, fingerprint, now.Add(time.Second)) {
		t.Fatal("second notifier should have independent failure state")
	}
}

func TestSendIncludesAppriseErrorDetails(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid notification type","notify_url":"discord://user:pass@discord.example/abcd"}`))
	}))
	defer server.Close()

	err := send(server.URL, "apprise://example.test", "Test Notification", "This is a test message", "info")
	if err == nil {
		t.Fatal("expected error")
	}

	got := err.Error()
	if !strings.Contains(got, "apprise request failed with status: 400 Bad Request") {
		t.Fatalf("expected status in error, got %q", got)
	}

	if !strings.Contains(got, "error: invalid notification type") {
		t.Fatalf("expected parsed error in error text, got %q", got)
	}

	if !strings.Contains(got, `"notify_url":"[REDACTED]"`) {
		t.Fatalf("expected response body to redact sensitive fields, got %q", got)
	}

	if strings.Contains(got, "discord://user:pass@discord.example/abcd") {
		t.Fatalf("expected sensitive url to be redacted, got %q", got)
	}
}

func TestSendIncludesTruncatedRawAppriseResponse(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("x", maxAppriseErrorResponseBodyBytes+100)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	err := send(server.URL, "apprise://example.test", "Test Notification", "This is a test message", "info")
	if err == nil {
		t.Fatal("expected error")
	}

	got := err.Error()
	if !strings.Contains(got, "(truncated)") {
		t.Fatalf("expected truncated marker in error text, got %q", got)
	}
}

func TestGetRevision(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		reference string
		commitSha string
		expected  string
	}{
		{
			name:      "Valid Revision",
			reference: "main",
			commitSha: "1234567890abcdef",
			expected:  "main (1234567890abcdef)",
		},
		{
			name:      "Empty Reference",
			reference: "",
			commitSha: "1234567890abcdef",
			expected:  "1234567890abcdef",
		},
		{
			name:      "Empty Commit SHA",
			reference: "main",
			commitSha: "",
			expected:  "main",
		},
		{
			name:      "Empty Both",
			reference: "",
			commitSha: "",
			expected:  "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := GetRevision(tc.reference, tc.commitSha)
			if result != tc.expected {
				t.Errorf("expected %s, got %s", tc.expected, result)
			}
		})
	}
}

// defaultBody renders the built-in notification body for a message + metadata,
// exercising the default template path (TemplateData.DefaultBody).
func defaultBody(message string, m Metadata) string {
	return TemplateData{Message: message, Metadata: m}.DefaultBody()
}

func TestDefaultBody(t *testing.T) {
	t.Parallel()

	t.Run("single-line message adds newline after first colon", func(t *testing.T) {
		t.Parallel()

		message := defaultBody("Deployment failed: timeout reached", Metadata{})
		expected := "Deployment failed: timeout reached"

		if message != expected {
			t.Errorf("expected %q, got %q", expected, message)
		}
	})

	t.Run("multi-line version message keeps inline versions", func(t *testing.T) {
		t.Parallel()

		message := defaultBody("Current Version: v0.80.0\nLatest Version: v0.80.1", Metadata{})
		expected := "Current Version: v0.80.0\nLatest Version: v0.80.1"

		if message != expected {
			t.Errorf("expected %q, got %q", expected, message)
		}
	})

	t.Run("duration is a metadata line when set, omitted when zero", func(t *testing.T) {
		t.Parallel()

		message := defaultBody("Deployment completed", Metadata{
			Repository: "acme/api",
			Duration:   12483 * time.Millisecond,
		})
		expected := "Deployment completed\n\nduration: 12.483s\nrepository: acme/api"

		if message != expected {
			t.Errorf("expected %q, got %q", expected, message)
		}

		message = defaultBody("Deployment completed", Metadata{Repository: "acme/api"})
		expected = "Deployment completed\n\nrepository: acme/api"

		if message != expected {
			t.Errorf("expected %q, got %q", expected, message)
		}
	})

	t.Run("reconciliation metadata includes event and affected actor", func(t *testing.T) {
		t.Parallel()

		message := defaultBody("Deployment triggered", Metadata{
			Repository:          "acme/api",
			Stack:               "prod",
			JobID:               "job-1",
			TraceID:             "trace-123",
			ReconciliationEvent: "unhealthy",
			AffectedActorKind:   "service",
			AffectedActorID:     "abc123def456",
			AffectedActorName:   "prod_api",
		})
		expected := "Deployment triggered\n\ncontext: default\nrepository: acme/api\nstack: prod\nreconciliation:\n  event: unhealthy\n  service_id: abc123def456\n  service_name: prod_api\n  trace_id: trace-123"

		if message != expected {
			t.Errorf("expected %q, got %q", expected, message)
		}
	})

	t.Run("named docker context is included", func(t *testing.T) {
		t.Parallel()

		message := defaultBody("Deployment completed", Metadata{
			Stack:   "prod",
			Context: "remote",
		})
		expected := "Deployment completed\n\ncontext: remote\nstack: prod"

		if message != expected {
			t.Errorf("expected %q, got %q", expected, message)
		}
	})

	t.Run("reconciliation-only metadata is nested under reconciliation", func(t *testing.T) {
		t.Parallel()

		message := defaultBody("Restart suppressed", Metadata{
			ReconciliationEvent: "unhealthy",
			AffectedActorKind:   "container",
			AffectedActorID:     "abc123",
			AffectedActorName:   "web_1",
		})
		expected := "Restart suppressed\n\nreconciliation:\n  container_id: abc123\n  container_name: web_1\n  event: unhealthy"

		if message != expected {
			t.Errorf("expected %q, got %q", expected, message)
		}
	})

	t.Run("metadata values remain unquoted", func(t *testing.T) {
		t.Parallel()

		message := defaultBody("Deploy done", Metadata{
			Repository: "acme/o'hara",
		})
		expected := "Deploy done\n\nrepository: acme/o'hara"

		if message != expected {
			t.Errorf("expected %q, got %q", expected, message)
		}
	})

	t.Run("non-reconciliation message keeps job id", func(t *testing.T) {
		t.Parallel()

		message := defaultBody("Deploy done", Metadata{
			Repository: "acme/repo",
			JobID:      "job-99",
		})
		expected := "Deploy done\n\njob_id: job-99\nrepository: acme/repo"

		if message != expected {
			t.Errorf("expected %q, got %q", expected, message)
		}
	})

	t.Run("unknown actor kind does not emit actor id or name keys", func(t *testing.T) {
		t.Parallel()

		message := defaultBody("Reconciled", Metadata{
			ReconciliationEvent: "update",
			AffectedActorKind:   "task",
			AffectedActorID:     "zzz111",
			AffectedActorName:   "ignored",
		})
		expected := "Reconciled\n\nreconciliation:\n  event: update"

		if message != expected {
			t.Errorf("expected %q, got %q", expected, message)
		}
	})
}

func TestValidateTemplate(t *testing.T) {
	t.Parallel()

	t.Run("empty template returns nil (use default)", func(t *testing.T) {
		t.Parallel()

		tmpl, err := validateTemplate("   ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if tmpl != nil {
			t.Errorf("expected nil template, got %v", tmpl)
		}
	})

	t.Run("valid template parses", func(t *testing.T) {
		t.Parallel()

		if _, err := validateTemplate("{{.Emoji}} {{.Title}} on {{.Target}}/{{.Stack}}"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("syntax error is rejected", func(t *testing.T) {
		t.Parallel()

		if _, err := validateTemplate("{{.Stack"); !errors.Is(err, ErrInvalidTemplate) {
			t.Fatalf("expected ErrInvalidTemplate, got: %v", err)
		}
	})

	t.Run("unknown field is rejected at config time", func(t *testing.T) {
		t.Parallel()

		if _, err := validateTemplate("{{.Nonexistent}}"); !errors.Is(err, ErrInvalidTemplate) {
			t.Fatalf("expected ErrInvalidTemplate, got: %v", err)
		}
	})
}

func TestRenderTemplate(t *testing.T) {
	t.Parallel()

	t.Run("renders metadata fields and trims trailing newlines", func(t *testing.T) {
		t.Parallel()

		tmpl, err := validateTemplate("{{.Emoji}} {{.Title}} — {{.Target}}/{{.Stack}} @ {{.Revision}}\n")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		got := renderTemplate(tmpl, Success, "Deployment completed", "ignored body", Metadata{
			Target:   "prod-vm",
			Stack:    "app",
			Revision: "main (abc123)",
			JobID:    "job-1",
		})
		expected := "✅ Deployment completed — prod-vm/app @ main (abc123)"

		if got != expected {
			t.Errorf("expected %q, got %q", expected, got)
		}
	})

	t.Run("exposes reconciliation flag", func(t *testing.T) {
		t.Parallel()

		tmpl, err := validateTemplate("{{if .IsReconciliation}}reconcile {{.ReconciliationEvent}}{{else}}deploy{{end}}")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		got := renderTemplate(tmpl, Warning, "Service restarted", "", Metadata{ReconciliationEvent: "unhealthy"})
		if got != "reconcile unhealthy" {
			t.Errorf("expected %q, got %q", "reconcile unhealthy", got)
		}
	})

	t.Run("nil template falls back to the built-in body", func(t *testing.T) {
		t.Parallel()

		got := renderTemplate(nil, Success, "Deployment completed", "Deploy done", Metadata{
			Repository: "acme/repo",
			JobID:      "job-99",
		})
		expected := "Deploy done\n\njob_id: job-99\nrepository: acme/repo"

		if got != expected {
			t.Errorf("expected %q, got %q", expected, got)
		}
	})

	t.Run("renders duration and changed services", func(t *testing.T) {
		t.Parallel()

		tmpl, err := validateTemplate("{{.Stack}} in {{.Duration}}{{range .ChangedServices}} {{.}}{{end}}")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		got := renderTemplate(tmpl, Success, "Deployment completed", "", Metadata{
			Stack:           "app",
			Duration:        1500 * time.Millisecond,
			ChangedServices: []string{"web", "worker"},
		})
		expected := "app in 1.5s web worker"

		if got != expected {
			t.Errorf("expected %q, got %q", expected, got)
		}
	})
}

func TestWithoutBodyTemplate(t *testing.T) {
	t.Parallel()

	var o sendOptions

	WithoutBodyTemplate()(&o)

	if !o.skipBodyTemplate {
		t.Fatal("expected WithoutBodyTemplate to set skipBodyTemplate; Send then renders the built-in body regardless of APPRISE_NOTIFY_BODY_TEMPLATE")
	}
}

func TestFormatTitle(t *testing.T) {
	t.Parallel()

	t.Run("regular title does not include reconciliation marker", func(t *testing.T) {
		t.Parallel()

		title := formatTitle(Success, "Deployment completed", Metadata{})
		expected := "✅ Deployment completed"

		if title != expected {
			t.Errorf("expected %q, got %q", expected, title)
		}
	})

	t.Run("reconciliation title includes short marker", func(t *testing.T) {
		t.Parallel()

		title := formatTitle(Success, "Deployment completed", Metadata{ReconciliationEvent: "unhealthy"})
		expected := "✅ [R] Deployment completed"

		if title != expected {
			t.Errorf("expected %q, got %q", expected, title)
		}
	})

	t.Run("whitespace reconciliation event is ignored", func(t *testing.T) {
		t.Parallel()

		title := formatTitle(Warning, "Service restarted", Metadata{ReconciliationEvent: "   "})
		expected := "⚠️ Service restarted"

		if title != expected {
			t.Errorf("expected %q, got %q", expected, title)
		}
	})
}
