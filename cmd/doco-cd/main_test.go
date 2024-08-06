package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"

	"github.com/google/uuid"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestHandleEvent(t *testing.T) {
	testCases := []struct {
		name                 string
		payload              webhook.ParsedPayload
		expectedStatusCode   int
		expectedResponseBody string
	}{
		{
			name: "Successful Deployment",
			payload: webhook.ParsedPayload{
				Ref:       "refs/heads/main",
				CommitSHA: "26263c2b44133367927cd1423d8c8457b5befce5",
				Name:      "doco-cd",
				FullName:  "kimdre/doco-cd",
				CloneURL:  "https://github.com/kimdre/doco-cd",
				Private:   false,
			},
			expectedStatusCode:   http.StatusCreated,
			expectedResponseBody: `{"details":"project deployment successful","job_id":"%s"}%s`,
		},
		{
			name: "Invalid Reference",
			payload: webhook.ParsedPayload{
				Ref:       "refs/heads/invalid",
				CommitSHA: "26263c2b44133367927cd1423d8c8457b5befce5",
				Name:      "doco-cd",
				FullName:  "kimdre/doco-cd",
				CloneURL:  "https://github.com/kimdre/doco-cd",
				Private:   false,
			},
			expectedStatusCode:   http.StatusInternalServerError,
			expectedResponseBody: `{"error":"failed to clone repository","details":"couldn't find remote ref \"refs/heads/invalid\"","job_id":"%s"}%s`,
		},
		{
			name: "Private Repository with Missing Access Token",
			payload: webhook.ParsedPayload{
				Ref:       "refs/heads/main",
				CommitSHA: "26263c2b44133367927cd1423d8c8457b5befce5",
				Name:      "doco-cd",
				FullName:  "kimdre/doco-cd",
				CloneURL:  "https://github.com/kimdre/doco-cd",
				Private:   true,
			},
			expectedStatusCode:   http.StatusInternalServerError,
			expectedResponseBody: `{"error":"missing access token for private repository","job_id":"%s"}%s`,
		},
		{
			name: "Private Repository with Missing Access Token",
			payload: webhook.ParsedPayload{
				Ref:       "refs/heads/main",
				CommitSHA: "26263c2b44133367927cd1423d8c8457b5befce5",
				Name:      "doco-cd",
				FullName:  "kimdre/doco-cd",
				CloneURL:  "https://github.com/kimdre/doco-cd",
				Private:   true,
			},
			expectedStatusCode:   http.StatusInternalServerError,
			expectedResponseBody: `{"error":"missing access token for private repository","job_id":"%s"}%s`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			appConfig, _ := config.GetAppConfig()

			log := logger.New(12)
			jobID := uuid.Must(uuid.NewRandom()).String()
			jobLog := log.With(slog.String("job_id", jobID))

			ctx := context.Background()

			dockerCli, err := docker.CreateDockerCli()
			if err != nil {
				if tc.expectedStatusCode == http.StatusInternalServerError {
					return
				}

				t.Fatalf("Failed to create docker client: %v", err)
			}

			t.Cleanup(func() {
				err = dockerCli.Client().Close()
				if err != nil {
					return
				}
			})

			t.Cleanup(func() {
				service := compose.NewComposeService(dockerCli)

				downOpts := api.DownOptions{
					RemoveOrphans: true,
					Images:        "all",
					Volumes:       true,
				}

				t.Log("Remove test container")

				err = service.Down(ctx, tc.payload.Name, downOpts)
				if err != nil {
					t.Fatal(err)
				}
			})

			err = docker.VerifySocketConnection(appConfig.DockerAPIVersion)
			if err != nil {
				t.Fatalf("Failed to verify docker socket connection: %v", err)
			}

			rr := httptest.NewRecorder()

			HandleEvent(
				ctx,
				jobLog,
				rr,
				appConfig,
				tc.payload,
				jobID,
				dockerCli,
			)

			if status := rr.Code; status != tc.expectedStatusCode {
				t.Errorf("handler returned wrong status code: got %v want %v",
					status, tc.expectedStatusCode)
			}

			expectedReturnMessage := fmt.Sprintf(tc.expectedResponseBody, jobID, "\n")
			if rr.Body.String() != expectedReturnMessage {
				t.Errorf("handler returned unexpected body: got '%v' want '%v'",
					rr.Body.String(), expectedReturnMessage)
			}
		})
	}
}

func TestJSONResponse(t *testing.T) {
	rr := httptest.NewRecorder()

	jobId := uuid.Must(uuid.NewRandom()).String()

	JSONResponse(rr, "this is a test", jobId, http.StatusOK)

	if rr.Code != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v",
			rr.Code, http.StatusOK)
	}

	expectedReturnMessage := fmt.Sprintf(`{"details":"this is a test","job_id":"%s"}%s`, jobId, "\n")
	if rr.Body.String() != expectedReturnMessage {
		t.Errorf("handler returned unexpected body: got '%v' want '%v'",
			rr.Body.String(), expectedReturnMessage)
	}
}

func TestJSONError(t *testing.T) {
	rr := httptest.NewRecorder()

	jobId := uuid.Must(uuid.NewRandom()).String()

	JSONError(rr, "this is a error", "this is a detail", jobId, http.StatusInternalServerError)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("handler returned wrong status code: got %v want %v",
			rr.Code, http.StatusInternalServerError)
	}

	expectedReturnMessage := fmt.Sprintf(`{"error":"this is a error","details":"this is a detail","job_id":"%s"}%s`, jobId, "\n")
	if rr.Body.String() != expectedReturnMessage {
		t.Errorf("handler returned unexpected body: got '%v' want '%v'",
			rr.Body.String(), expectedReturnMessage)
	}
}
