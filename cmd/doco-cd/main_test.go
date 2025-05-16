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

	"github.com/kimdre/doco-cd/internal/git"

	"github.com/docker/docker/client"

	"github.com/docker/docker/api/types/container"

	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"

	"github.com/google/uuid"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/webhook"
)

const (
	validCommitSHA   = "26263c2b44133367927cd1423d8c8457b5befce5"
	invalidCommitSHA = "1111111111111111111111111111111111111111"
	projectName      = "compose-webhook"
	invalidBranch    = "refs/heads/invalid"
)

func TestMain(m *testing.M) {
	// Set up any necessary environment variables or configurations here
	// For example, you might want to set a default log level or HTTP port
	// Run the tests
	exitCode := m.Run()

	// Clean up any resources or configurations here

	// Exit with the appropriate code
	os.Exit(exitCode)
}

func TestHandleEvent(t *testing.T) {
	defaultEnvVars := map[string]string{
		"GIT_ACCESS_TOKEN": os.Getenv("GIT_ACCESS_TOKEN"),
		"WEBHOOK_SECRET":   os.Getenv("WEBHOOK_SECRET"),
	}

	testCases := []struct {
		name                 string
		payload              webhook.ParsedPayload
		expectedStatusCode   int
		expectedResponseBody string
		overrideEnv          map[string]string
		customTarget         string
	}{
		{
			name: "Successful Deployment",
			payload: webhook.ParsedPayload{
				Ref:       git.MainBranch,
				CommitSHA: validCommitSHA,
				Name:      projectName,
				FullName:  "kimdre/doco-cd",
				CloneURL:  "https://github.com/kimdre/doco-cd",
				Private:   false,
			},
			expectedStatusCode:   http.StatusCreated,
			expectedResponseBody: `{"details":"job completed successfully","job_id":"%[1]s"}`,
			overrideEnv:          nil,
			customTarget:         "",
		},
		{
			name: "Successful Deployment with custom Target",
			payload: webhook.ParsedPayload{
				Ref:       git.MainBranch,
				CommitSHA: "f291bfca73b06814293c1f9c9f3c7f95e4932564",
				Name:      projectName,
				FullName:  "kimdre/doco-cd",
				CloneURL:  "https://github.com/kimdre/doco-cd",
				Private:   false,
			},
			expectedStatusCode:   http.StatusCreated,
			expectedResponseBody: `{"details":"job completed successfully","job_id":"%[1]s"}`,
			overrideEnv:          nil,
			customTarget:         "test",
		},
		{
			name: "Invalid Branch",
			payload: webhook.ParsedPayload{
				Ref:       invalidBranch,
				CommitSHA: validCommitSHA,
				Name:      projectName,
				FullName:  "kimdre/doco-cd",
				CloneURL:  "https://github.com/kimdre/doco-cd",
				Private:   false,
			},
			expectedStatusCode:   http.StatusInternalServerError,
			expectedResponseBody: `{"error":"failed to clone repository","details":"couldn't find remote ref \"` + invalidBranch + `\"","job_id":"%[1]s"}`,
			overrideEnv:          nil,
			customTarget:         "",
		},
		{
			name: "Private Repository",
			payload: webhook.ParsedPayload{
				Ref:       git.MainBranch,
				CommitSHA: validCommitSHA,
				Name:      projectName,
				FullName:  "kimdre/doco-cd",
				CloneURL:  "https://github.com/kimdre/doco-cd",
				Private:   true,
			},
			expectedStatusCode:   http.StatusCreated,
			expectedResponseBody: `{"details":"job completed successfully","job_id":"%[1]s"}`,
			overrideEnv:          nil,
			customTarget:         "",
		},
		{
			name: "Missing Deployment Configuration",
			payload: webhook.ParsedPayload{
				Ref:       git.MainBranch,
				CommitSHA: "efefb4111f3c363692a2526f9be9b24560e6511f",
				Name:      projectName,
				FullName:  "kimdre/kimdre",
				CloneURL:  "https://github.com/kimdre/kimdre",
				Private:   false,
			},
			expectedStatusCode:   http.StatusInternalServerError,
			expectedResponseBody: `{"error":"no compose files found: stat %[2]s/docker-compose.yaml: no such file or directory","details":"deployment failed","job_id":"%[1]s"}`,
			overrideEnv:          nil,
			customTarget:         "",
		},
		{
			name: "With Remote Repository",
			payload: webhook.ParsedPayload{
				Ref:       "remote",
				CommitSHA: validCommitSHA,
				Name:      projectName,
				FullName:  "kimdre/test",
				CloneURL:  "https://github.com/kimdre/test",
				Private:   true,
			},
			expectedStatusCode:   http.StatusCreated,
			expectedResponseBody: `{"details":"job completed successfully","job_id":"%[1]s"}`,
			overrideEnv:          nil,
			customTarget:         "",
		},
	}

	// Restore environment variables after the test
	for _, k := range []string{"LOG_LEVEL", "HTTP_PORT", "WEBHOOK_SECRET", "GIT_ACCESS_TOKEN", "AUTH_TYPE", "SKIP_TLS_VERIFICATION"} {
		if v, ok := os.LookupEnv(k); ok {
			t.Cleanup(func() {
				err := os.Setenv(k, v)
				if err != nil {
					t.Fatalf("failed to restore environment variable %s: %v", k, err)
				}
			})
		}
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()

			for k, v := range defaultEnvVars {
				err := os.Setenv(k, v)
				if err != nil {
					t.Fatalf("Failed to set environment variable: %v", err)
				}

				t.Cleanup(func() {
					err = os.Unsetenv(k)
					if err != nil {
						t.Fatalf("Failed to unset environment variable: %v", err)
					}
				})
			}

			if tc.overrideEnv != nil {
				for k, v := range tc.overrideEnv {
					err := os.Setenv(k, v)
					if err != nil {
						t.Fatalf("Failed to set environment variable: %v", err)
					}
				}
			}

			appConfig, err := config.GetAppConfig()
			if err != nil {
				t.Fatalf("failed to get app config: %s", err.Error())
			}

			dockerClient, _ := client.NewClientWithOpts(
				client.FromEnv,
				client.WithAPIVersionNegotiation(),
			)

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

			err = docker.VerifySocketConnection()
			if err != nil {
				t.Fatalf("Failed to verify docker socket connection: %v", err)
			}

			rr := httptest.NewRecorder()

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

			testMountPoint := container.MountPoint{
				Type:        "bind",
				Source:      tmpDir,
				Destination: tmpDir,
				Mode:        "rw",
			}

			HandleEvent(
				ctx,
				jobLog,
				rr,
				appConfig,
				testMountPoint,
				tc.payload,
				tc.customTarget,
				jobID,
				dockerCli,
				dockerClient,
			)

			if status := rr.Code; status != tc.expectedStatusCode {
				t.Errorf("handler returned wrong status code: got %v want %v",
					status, tc.expectedStatusCode)
			}

			expectedReturnMessage := fmt.Sprintf(tc.expectedResponseBody, jobID, filepath.Join(tmpDir, getRepoName(tc.payload.CloneURL))) + "\n"
			if rr.Body.String() != expectedReturnMessage {
				t.Errorf("handler returned unexpected body: got '%v' want '%v'",
					rr.Body.String(), expectedReturnMessage)
			}
		})
	}
}
