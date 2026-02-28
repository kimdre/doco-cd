package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/google/uuid"

	"github.com/kimdre/doco-cd/internal/test"

	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/secretprovider"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/webhook"
)

const (
	validCommitSHA = "26263c2b44133367927cd1423d8c8457b5befce5"
	invalidBranch  = "refs/heads/invalid"
)

var WorkingDir string

func TestMain(m *testing.M) {
	var err error

	WorkingDir, err = os.Getwd()
	if err != nil {
		log.Fatalf("os.Getwd: %v", err)
	}

	log.Println("working dir:", WorkingDir)

	ctx := context.Background()

	dockerCli, err := docker.CreateDockerCli(false, false)
	if err != nil {
		log.Fatalf("Failed to create docker client: %v", err)
	}

	err = docker.VerifySocketConnection()
	if err != nil {
		log.Fatalf("Failed to verify docker socket connection: %v", err)
	}

	swarm.ModeEnabled, err = swarm.CheckDaemonIsSwarmManager(ctx, dockerCli)
	if err != nil {
		log.Fatalf("Failed to check if Docker daemon is in Swarm mode: %v", err)
	}

	if swarm.ModeEnabled {
		log.Println("Testing in Docker Swarm mode")
	} else {
		log.Println("Testing in Docker Standalone mode")
	}

	// Ensure the Docker client is closed after tests
	defer func() {
		if err := dockerCli.Client().Close(); err != nil {
			log.Printf("Failed to close Docker client: %v", err)
		}
	}()

	m.Run()
}

func TestHandleEvent(t *testing.T) {
	testCases := []struct {
		name                 string
		payload              webhook.ParsedPayload
		expectedStatusCode   int
		expectedResponseBody string
		customTarget         string
		swarmMode            bool
	}{
		{
			name: "Successful Deployment",
			payload: webhook.ParsedPayload{
				Ref:       git.MainBranch,
				CommitSHA: validCommitSHA,
				Name:      "doco-cd",
				FullName:  "kimdre/doco-cd",
				CloneURL:  "https://github.com/kimdre/doco-cd",
				Private:   false,
			},
			expectedStatusCode:   http.StatusCreated,
			expectedResponseBody: `{"content":"job completed successfully","job_id":"%[1]s"}`,
			customTarget:         "",
			swarmMode:            false,
		},
		{
			name: "Successful Deployment with custom Target",
			payload: webhook.ParsedPayload{
				Ref:       git.MainBranch,
				CommitSHA: "f291bfca73b06814293c1f9c9f3c7f95e4932564",
				Name:      "doco-cd",
				FullName:  "kimdre/doco-cd",
				CloneURL:  "https://github.com/kimdre/doco-cd",
				Private:   false,
			},
			expectedStatusCode:   http.StatusCreated,
			expectedResponseBody: `{"content":"job completed successfully","job_id":"%[1]s"}`,
			customTarget:         "test",
			swarmMode:            false,
		},
		{
			name: "Invalid Reference",
			payload: webhook.ParsedPayload{
				Ref:       invalidBranch,
				CommitSHA: validCommitSHA,
				Name:      "doco-cd",
				FullName:  "kimdre/doco-cd",
				CloneURL:  "https://github.com/kimdre/doco-cd",
				Private:   false,
			},
			expectedStatusCode:   http.StatusInternalServerError,
			expectedResponseBody: `{"error":"failed to clone repository","content":"failed to checkout repository: failed to get reference set: invalid reference, should be a tag or a branch: ` + invalidBranch + `","job_id":"%[1]s"}`,
			customTarget:         "",
			swarmMode:            false,
		},
		{
			name: "Private Repository",
			payload: webhook.ParsedPayload{
				Ref:       git.MainBranch,
				CommitSHA: validCommitSHA,
				Name:      "doco-cd",
				FullName:  "kimdre/doco-cd",
				CloneURL:  "https://github.com/kimdre/doco-cd",
				Private:   true,
			},
			expectedStatusCode:   http.StatusCreated,
			expectedResponseBody: `{"content":"job completed successfully","job_id":"%[1]s"}`,
			customTarget:         "",
			swarmMode:            false,
		},
		{
			name: "Missing Compose Configuration",
			payload: webhook.ParsedPayload{
				Ref:       git.MainBranch,
				CommitSHA: "efefb4111f3c363692a2526f9be9b24560e6511f",
				Name:      "kimdre",
				FullName:  "kimdre/kimdre",
				CloneURL:  "https://github.com/kimdre/kimdre",
				Private:   false,
			},
			expectedStatusCode:   http.StatusInternalServerError,
			expectedResponseBody: `{"error":"deployment failed","content":"failed to deploy stack %[3]s: no compose files found: stat %[2]s/docker-compose.yaml: no such file or directory","job_id":"%[1]s"}`,
			customTarget:         "",
			swarmMode:            false,
		},
		{
			name: "With Remote Repository",
			payload: webhook.ParsedPayload{
				Ref:       "remote",
				CommitSHA: "d02f87d2a886d6bae4673409f6b5108b45156f5c",
				Name:      "doco-cd_tests",
				FullName:  "kimdre/doco-cd_tests",
				CloneURL:  "https://github.com/kimdre/doco-cd_tests",
				Private:   false,
			},
			expectedStatusCode:   http.StatusCreated,
			expectedResponseBody: `{"content":"job completed successfully","job_id":"%[1]s"}`,
			customTarget:         "",
			swarmMode:            false,
		},
		{
			name: "With Swarm Mode",
			payload: webhook.ParsedPayload{
				Ref:       git.SwarmModeBranch,
				CommitSHA: "01435dad4e7ff8f7da70202ca1ca77bccca9eb62",
				Name:      "doco-cd_tests",
				FullName:  "kimdre/doco-cd_tests",
				CloneURL:  "https://github.com/kimdre/doco-cd_tests",
				Private:   false,
			},
			expectedStatusCode:   http.StatusCreated,
			expectedResponseBody: `{"content":"job completed successfully","job_id":"%[1]s"}`,
			customTarget:         "",
			swarmMode:            true,
		},
	}

	appConfig, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("failed to get app config: %s", err.Error())
	}

	dockerCli, err := docker.CreateDockerCli(appConfig.DockerQuietDeploy, !appConfig.SkipTLSVerification)
	if err != nil {
		t.Fatalf("Failed to create Docker CLI: %v", err)
	}

	swarm.ModeEnabled, err = swarm.CheckDaemonIsSwarmManager(t.Context(), dockerCli)
	if err != nil {
		log.Fatalf("Failed to check if Docker daemon is in Swarm mode: %v", err)
	}

	dockerClient, _ := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)

	encryption.SetupAgeKeyEnvVar(t)

	defaultEnvVars := map[string]string{
		"GIT_ACCESS_TOKEN": os.Getenv("GIT_ACCESS_TOKEN"),
		"WEBHOOK_SECRET":   os.Getenv("WEBHOOK_SECRET"),
	}

	for k, v := range defaultEnvVars {
		t.Setenv(k, v)
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if swarm.ModeEnabled != tc.swarmMode {
				t.Skipf("Skipping test because it requires swarm mode %v, but current mode is %v", tc.swarmMode, swarm.ModeEnabled)
			}

			tmpDir := t.TempDir()

			stackName := test.ConvertTestName(t.Name())
			if len(stackName) > 40 {
				stackName = stackName[:40]
			}

			if tc.payload.Private && appConfig.GitAccessToken == "" {
				t.Skip("Skipping test for private repository because GIT_ACCESS_TOKEN is not set")
			}

			log := logger.New(12)
			jobID := uuid.Must(uuid.NewV7()).String()
			jobLog := log.With(slog.String("job_id", jobID))

			ctx := context.Background()

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

			secretProvider, err := secretprovider.Initialize(ctx, appConfig.SecretProvider, "v0.0.0-test")
			if err != nil {
				t.Fatalf("failed to initialize secret provider: %s", err.Error())

				return
			}

			if secretProvider != nil {
				t.Cleanup(func() {
					secretProvider.Close()
				})
			}

			rr := httptest.NewRecorder()

			t.Cleanup(func() {
				service := compose.NewComposeService(dockerCli)

				downOpts := api.DownOptions{
					RemoveOrphans: true,
					Images:        "all",
					Volumes:       true,
				}

				if swarm.ModeEnabled {
					err = docker.RemoveSwarmStack(ctx, dockerCli, stackName)
				} else if service != nil {
					err = service.Down(ctx, stackName, downOpts)
				}

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
				&secretProvider,
				stackName,
			)

			if status := rr.Code; status != tc.expectedStatusCode {
				t.Errorf("handler returned wrong status code: got %v want %v",
					status, tc.expectedStatusCode)
			}

			expectedReturnMessage := fmt.Sprintf(tc.expectedResponseBody, jobID, filepath.Join(tmpDir, git.GetRepoName(tc.payload.CloneURL)), stackName) + "\n"
			if rr.Body.String() != expectedReturnMessage {
				t.Errorf("handler returned unexpected body: got '%v' want '%v'",
					rr.Body.String(), expectedReturnMessage)
			}
		})
	}
}

func TestGetProxyUrlRedacted(t *testing.T) {
	t.Parallel()

	// Test cases with different proxy URLs
	testCases := []struct {
		name     string
		proxyURL string
		expected string
	}{
		{
			name:     "Valid HTTP Proxy",
			proxyURL: "http://user:password@proxy:8080", // #nosec G101
			expected: "http://user:***@proxy:8080",
		},
		{
			name:     "Valid HTTPS Proxy",
			proxyURL: "https://user:password@proxy:8443", // #nosec G101
			expected: "https://user:***@proxy:8443",
		},
		{
			name:     "No Proxy URL",
			proxyURL: "",
			expected: "",
		},
		{
			name:     "Invalid Proxy URL",
			proxyURL: "not-a-valid-url",
			expected: "not-a-valid-url",
		},
		{
			name:     "Proxy URL with no credentials",
			proxyURL: "http://proxy:8080",
			expected: "http://proxy:8080",
		},
		{
			name:     "Proxy URL with empty credentials",
			proxyURL: "http://:@proxy:8080",
			expected: "http://:@proxy:8080",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := GetProxyUrlRedacted(tc.proxyURL)
			if result != tc.expected {
				t.Errorf("GetProxyUrlRedacted(%q) = %q; want %q", tc.proxyURL, result, tc.expected)
			}
		})
	}
}

func TestCreateMountpointSymlink(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		source      string
		destination string
		expectError error
	}{
		{
			name:        "Valid Symlink Creation",
			source:      "source",
			destination: "destination",
			expectError: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tmpDir := t.TempDir()

			err := CreateMountpointSymlink(container.MountPoint{
				Type:        "bind",
				Source:      filepath.Join(tmpDir, tc.source),
				Destination: filepath.Join(tmpDir, tc.destination),
				Mode:        "rw",
			})
			if !errors.Is(err, tc.expectError) {
				t.Errorf("symlink creation error: got %v, want %v", err, tc.expectError)
			}
		})
	}
}
