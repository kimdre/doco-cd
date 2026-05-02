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
		expected := "Deployment failed:\ntimeout reached\n"

		if message != expected {
			t.Errorf("expected %q, got %q", expected, message)
		}
	})

	t.Run("multi-line version message keeps inline versions", func(t *testing.T) {
		t.Parallel()

		message := formatMessage("Current Version: v0.80.0\nLatest Version: v0.80.1", Metadata{})
		expected := "Current Version: v0.80.0\nLatest Version: v0.80.1\n"

		if message != expected {
			t.Errorf("expected %q, got %q", expected, message)
		}
	})

	t.Run("reconciliation metadata includes event and affected actor", func(t *testing.T) {
		t.Parallel()

		message := formatMessage("Deployment triggered", Metadata{
			Repository:          "acme/api",
			Stack:               "prod",
			ReconciliationEvent: "unhealthy",
			AffectedActorKind:   "service",
			AffectedActorID:     "abc123def456",
			AffectedActorName:   "prod_api",
		})
		expected := "Deployment triggered\n\nrepository: acme/api\nstack: prod\nreconciliation_event: unhealthy\naffected_actor_id: abc123def456\naffected_actor_name: prod_api\naffected_actor_kind: service"

		if message != expected {
			t.Errorf("expected %q, got %q", expected, message)
		}
	})
}
