package notification

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/kimdre/doco-cd/internal/test"
)

func TestSend(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		appriseUrl    string
		expectedError string
	}{
		{
			name:          "Valid Service URL",
			appriseUrl:    "apprise://%s",
			expectedError: "",
		},
		{
			name:          "Invalid Service URL",
			appriseUrl:    "pover://wrong@test",
			expectedError: "failed to send notification: " + ErrNotifyFailed.Error(),
		},
	}

	ctx := context.Background()
	metadata := Metadata{
		Repository: "test",
		Stack:      "test-stack",
		Revision:   "main",
		JobID:      uuid.Must(uuid.NewV7()).String(),
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
			SetAppriseConfig("http://"+endpoint+"/notify", fmt.Sprint(tc.appriseUrl, endpoint), "info")

			err := Send(Info, "Test Notification", "This is a test message", metadata)
			if err != nil {
				if tc.expectedError == "" {
					t.Errorf("unexpected error: %v", err)
				} else if err.Error() != tc.expectedError {
					t.Errorf("expected error: %s, got: %s", tc.expectedError, err.Error())
				}
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
