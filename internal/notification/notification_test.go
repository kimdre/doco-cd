package notification

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/kimdre/doco-cd/internal/test"
	"github.com/kimdre/doco-cd/internal/utils/id"
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
			appriseURL: "apprise://%s",
			// nil means success is expected
			expectedErr: nil,
		},
		{
			name:        "Invalid Service URL",
			appriseURL:  "pover://wrong@test",
			expectedErr: ErrNotifyFailed,
		},
	}

	ctx := context.Background()
	metadata := Metadata{
		Repository: "test",
		Stack:      "test-stack",
		Revision:   "main",
		JobID:      id.GenID(),
	}

	appriseComposeYAML := `services:
  apprise:
    image: caronc/apprise:latest
    ports:
      - "8000"
    environment:
      APPRISE_WORKER_COUNT: "1"
    healthcheck:
      test: ["CMD-SHELL", "curl -fsS http://127.0.0.1:8000/status >/dev/null || exit 1"]
      interval: 2s
      timeout: 5s
      retries: 10
`
	stack := test.ComposeUp(ctx, t, test.WithYAML(appriseComposeYAML))
	endpoint := stack.Endpoint(ctx, t, "apprise", "8000")

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Cannot run tests in parallel because SetAppriseConfig modifies global variables
			SetAppriseConfig("http://"+endpoint+"/notify", fmt.Sprintf(tc.appriseURL, endpoint), "info")

			err := Send(Info, "Test Notification", "This is a test message", metadata)
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

func TestFormatMessage(t *testing.T) {
	t.Parallel()

	t.Run("single-line message adds newline after first colon", func(t *testing.T) {
		t.Parallel()

		message := formatMessage("Deployment failed: timeout reached", Metadata{})
		expected := "Deployment failed: timeout reached"

		if message != expected {
			t.Errorf("expected %q, got %q", expected, message)
		}
	})

	t.Run("multi-line version message keeps inline versions", func(t *testing.T) {
		t.Parallel()

		message := formatMessage("Current Version: v0.80.0\nLatest Version: v0.80.1", Metadata{})
		expected := "Current Version: v0.80.0\nLatest Version: v0.80.1"

		if message != expected {
			t.Errorf("expected %q, got %q", expected, message)
		}
	})

	t.Run("reconciliation metadata includes event and affected actor", func(t *testing.T) {
		t.Parallel()

		message := formatMessage("Deployment triggered", Metadata{
			Repository:          "acme/api",
			Stack:               "prod",
			JobID:               "job-1",
			TraceID:             "trace-123",
			ReconciliationEvent: "unhealthy",
			AffectedActorKind:   "service",
			AffectedActorID:     "abc123def456",
			AffectedActorName:   "prod_api",
		})
		expected := "Deployment triggered\n\nrepository: acme/api\nstack: prod\nreconciliation:\n  event: unhealthy\n  service_id: abc123def456\n  service_name: prod_api\n  trace_id: trace-123"

		if message != expected {
			t.Errorf("expected %q, got %q", expected, message)
		}
	})

	t.Run("reconciliation-only metadata is nested under reconciliation", func(t *testing.T) {
		t.Parallel()

		message := formatMessage("Restart suppressed", Metadata{
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

		message := formatMessage("Deploy done", Metadata{
			Repository: "acme/o'hara",
		})
		expected := "Deploy done\n\nrepository: acme/o'hara"

		if message != expected {
			t.Errorf("expected %q, got %q", expected, message)
		}
	})

	t.Run("non-reconciliation message keeps job id", func(t *testing.T) {
		t.Parallel()

		message := formatMessage("Deploy done", Metadata{
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

		message := formatMessage("Reconciled", Metadata{
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
