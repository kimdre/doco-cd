package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"

	"github.com/google/uuid"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/webhook"
)

var (
	validCommitSHA = "26263c2b44133367927cd1423d8c8457b5befce5"
	projectName    = "doco-cd"
	mainBranch     = "refs/heads/main"
	invalidBranch  = "refs/heads/invalid"
)

func TestHandleEvent(t *testing.T) {
	testCases := []struct {
		name                 string
		payload              webhook.ParsedPayload
		expectedStatusCode   int
		expectedResponseBody string
		overrideEnv          map[string]string
	}{
		{
			name: "Successful Deployment",
			payload: webhook.ParsedPayload{
				Ref:       mainBranch,
				CommitSHA: validCommitSHA,
				Name:      projectName,
				FullName:  "kimdre/doco-cd",
				CloneURL:  "https://github.com/kimdre/doco-cd",
				Private:   false,
			},
			expectedStatusCode:   http.StatusCreated,
			expectedResponseBody: `{"details":"project deployment successful","job_id":"%s"}`,
			overrideEnv:          nil,
		},
		{
			name: "Invalid Reference",
			payload: webhook.ParsedPayload{
				Ref:       invalidBranch,
				CommitSHA: validCommitSHA,
				Name:      projectName,
				FullName:  "kimdre/doco-cd",
				CloneURL:  "https://github.com/kimdre/doco-cd",
				Private:   false,
			},
			expectedStatusCode:   http.StatusInternalServerError,
			expectedResponseBody: `{"error":"failed to clone repository","details":"couldn't find remote ref \"` + invalidBranch + `\"","job_id":"%s"}`,
			overrideEnv:          nil,
		},
		{
			name: "Private Repository",
			payload: webhook.ParsedPayload{
				Ref:       mainBranch,
				CommitSHA: validCommitSHA,
				Name:      projectName,
				FullName:  "kimdre/doco-cd",
				CloneURL:  "https://github.com/kimdre/doco-cd",
				Private:   true,
			},
			expectedStatusCode:   http.StatusCreated,
			expectedResponseBody: `{"details":"project deployment successful","job_id":"%s"}`,
			overrideEnv:          nil,
		},
		{
			name: "Private Repository with missing Git Access Token",
			payload: webhook.ParsedPayload{
				Ref:       mainBranch,
				CommitSHA: validCommitSHA,
				Name:      projectName,
				FullName:  "kimdre/doco-cd",
				CloneURL:  "https://github.com/kimdre/doco-cd",
				Private:   true,
			},
			expectedStatusCode:   http.StatusInternalServerError,
			expectedResponseBody: `{"error":"missing access token for private repository","job_id":"%s"}`,
			overrideEnv: map[string]string{
				"GIT_ACCESS_TOKEN": "",
			},
		},
		{
			name: "Missing Deployment Configuration",
			payload: webhook.ParsedPayload{
				Ref:       mainBranch,
				CommitSHA: "efefb4111f3c363692a2526f9be9b24560e6511f",
				Name:      projectName,
				FullName:  "kimdre/kimdre",
				CloneURL:  "https://github.com/kimdre/kimdre",
				Private:   false,
			},
			expectedStatusCode:   http.StatusInternalServerError,
			expectedResponseBody: `{"error":"no compose files found","details":"stat ` + filepath.Join(os.TempDir(), "kimdre/kimdre/docker-compose.yaml") + `: no such file or directory","job_id":"%s"}`,
			overrideEnv:          nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.overrideEnv != nil {
				for k, v := range tc.overrideEnv {
					err := os.Setenv(k, v)
					if err != nil {
						t.Fatalf("Failed to set environment variable: %v", err)
					}
				}
			}

			appConfig, _ := config.GetAppConfig()

			log := logger.New(12)
			jobID := uuid.Must(uuid.NewRandom()).String()
			jobLog := log.With(slog.String("job_id", jobID))

			ctx := context.Background()

			dockerCli, err := docker.CreateDockerCli(appConfig.DockerQuietDeploy, !appConfig.SkipTLSVerification)
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

			expectedReturnMessage := fmt.Sprintf(tc.expectedResponseBody, jobID) + "\n"
			if rr.Body.String() != expectedReturnMessage {
				t.Errorf("handler returned unexpected body: got '%v' want '%v'",
					rr.Body.String(), expectedReturnMessage)
			}
		})
	}
}
