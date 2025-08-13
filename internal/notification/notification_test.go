package notification

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestSend(t *testing.T) {
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
			expectedError: "failed to send notification: apprise request failed with status: 424 Failed Dependency",
		},
	}

	ctx := context.Background()
	metadata := Metadata{
		Repository: "test",
		Stack:      "test-stack",
		Revision:   "main",
		JobID:      uuid.Must(uuid.NewRandom()).String(),
	}

	ctr, err := testcontainers.Run(
		ctx,
		"caronc/apprise:latest",
		testcontainers.WithExposedPorts("8000/tcp"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/").WithPort("8000/tcp")),
		testcontainers.WithEnv(map[string]string{
			"APPRISE_WORKER_COUNT": "1",
		}),
	)
	if err != nil {
		t.Fatalf("failed to start apprise container: %v", err)
	}

	t.Cleanup(func() {
		if err = ctr.Terminate(ctx); err != nil {
			t.Errorf("failed to terminate apprise container: %v", err)
		}
	})

	state, err := ctr.State(ctx)
	if err != nil {
		t.Fatalf("failed to get container state: %v", err)
	}

	if !state.Running {
		t.Fatalf("expected container to be running, got %s", state.Status)
	}

	endpoint, err := ctr.Endpoint(ctx, "")
	if err != nil {
		t.Fatalf("failed to get endpoint: %v", err)
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			SetAppriseConfig("http://"+endpoint+"/notify", fmt.Sprint(tc.appriseUrl, endpoint), "info")

			err = Send(Info, "Test Notification", "This is a test message", metadata)
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
