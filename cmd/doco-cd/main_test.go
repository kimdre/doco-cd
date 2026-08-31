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
	"strings"
	"testing"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/common/id"

	"github.com/kimdre/doco-cd/internal/config/app"

	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider/bitwardensecretsmanager"
	"github.com/kimdre/doco-cd/internal/test"

	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/secretprovider"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/reconciliation"
	"github.com/kimdre/doco-cd/internal/webhook"
)

const (
	invalidBranch        = "refs/heads/invalid"
	localFixtureCloneURL = "https://fixture.invalid/kimdre/doco-cd-fixture"
	localFixtureRepoName = "doco-cd-fixture"
	localFixtureFullName = "kimdre/doco-cd-fixture"
	// remoteFixtureCommit is a self-contained commit on the "remote" branch of
	// the external kimdre/doco-cd_tests repository, used by the "Private
	// Repository" and "With Remote Repository" subtests below.
	remoteFixtureCommit = "ee6dda09a7cef86ace9e5991dcf3c4b9a56716d3"
)

// selfDeployComposeYAML backs the local, network-independent fixture repos
// used by the "Successful Deployment", "Successful Deployment with custom
// Target" and "Invalid Reference" subtests of TestHandleEvent.
const selfDeployComposeYAML = `
services:
  app:
    image: nginx:latest
    ports:
      - "80"  # use random published port
    depends_on:
      - dep
    volumes:
      - ./:/usr/share/nginx/html
    environment:
      TEST_ENV_VAR: example_value
    labels:
      - something=${SOMETHING:-none}
      - base=${BASE:-none}
      - prod=${PROD:-none}
      - remote=${REMOTE:-none}
    env_file:
      - test.env
    secrets:
      - test_secret
    configs:
      - test_config

  dep:
    image: nginx:latest

secrets:
  test_secret:
    file: ./secret.txt

configs:
  test_config:
    file: ./config.conf
`

// selfDeployFixtureFiles returns the fixture files needed to deploy
// selfDeployComposeYAML, rooted at dir (e.g. "" or "fixture/").
// An empty string dir means the files are in the root of the repository, while
// "fixture/" means they are in the "fixture" subdirectory of the repository.
func selfDeployFixtureFiles(dir string) map[string]string {
	return map[string]string{
		dir + "test.compose.yaml": selfDeployComposeYAML,
		dir + "test.env":          "SOMETHING=hello\nREMOTE=remote\n",
		dir + "config.conf":       "This is a config.",
		dir + "secret.txt":        "This is a secret.",
		dir + "index.html":        "It works.",
	}
}

var WorkingDir string

func TestMain(m *testing.M) {
	var err error

	WorkingDir, err = os.Getwd()
	if err != nil {
		log.Fatalf("os.Getwd: %v", err)
	}

	log.Println("working dir:", WorkingDir)

	ctx := context.Background()

	dockerCli, err := docker.CreateDockerCli(false)
	if err != nil {
		log.Fatalf("Failed to create docker client: %v", err)
	}

	err = docker.VerifySocketConnection()
	if err != nil {
		log.Fatalf("Failed to verify docker socket connection: %v", err)
	}

	if err := swarm.RefreshModeEnabled(ctx, dockerCli.Client()); err != nil {
		log.Fatalf("Failed to check if Docker daemon is in Swarm mode: %v", err)
	}

	if swarm.GetModeEnabled() {
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
	// Local, network-independent fixtures backing the subtests that deploy
	// this repository itself. "remoteTargetURL" stands in for the
	// "repository_url" deployment target that "selfDeployURL" points at
	// under the "test" custom target, also exercising "remote:"-prefixed
	// env file merging (base.env/prod.env local, test.env from the target).
	_, remoteTargetURL, _ := newLocalFixtureRepo(t, mergeFiles(
		map[string]string{
			".doco-cd.yaml": `
name: test-deploy-remote-target
reference: refs/heads/main
working_dir: .
compose_files:
  - test.compose.yaml
`,
		},
		selfDeployFixtureFiles(""),
	))

	_, selfDeployURL, selfDeployHash := newLocalFixtureRepo(t, mergeFiles(
		map[string]string{
			".doco-cd.yaml": `
name: test-deploy
reference: refs/heads/main
working_dir: fixture
compose_files:
  - test.compose.yaml
`,
			// base.env and prod.env are local to this repo and get merged
			// with test.env from the repository_url target (remoteTargetURL,
			// via the "remote:" prefix) to exercise cross-repo env file
			// merging entirely offline.
			".doco-cd.test.yaml": `
name: test-deploy-custom-target
repository_url: ` + remoteTargetURL + `
working_dir: .
compose_files:
  - test.compose.yaml
env_files:
  - base.env
  - prod.env
  - remote:test.env
`,
			"base.env": "SOMETHING=base\nBASE=base\n",
			"prod.env": "SOMETHING=prod\nPROD=prod\n",
		},
		selfDeployFixtureFiles("fixture/"),
	))

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
				CommitSHA: selfDeployHash,
				Name:      localFixtureRepoName,
				FullName:  localFixtureFullName,
				CloneURL:  localFixtureCloneURL,
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
				CommitSHA: selfDeployHash,
				Name:      localFixtureRepoName,
				FullName:  localFixtureFullName,
				CloneURL:  localFixtureCloneURL,
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
				CommitSHA: selfDeployHash,
				Name:      localFixtureRepoName,
				FullName:  localFixtureFullName,
				CloneURL:  localFixtureCloneURL,
				Private:   false,
			},
			expectedStatusCode:   http.StatusInternalServerError,
			expectedResponseBody: `{"error":"failed to clone repository","content":"failed to checkout repository: failed to get reference set: invalid reference, should be a tag or a branch: ` + invalidBranch + `","job_id":"%[1]s"}`,
			customTarget:         "",
			swarmMode:            false,
		},
		{
			// Kept as a real network clone (with GIT_ACCESS_TOKEN auth) to
			// exercise the Private-repository code path end-to-end, reusing
			// the "remote" branch/commit already validated by the
			// "With Remote Repository" subtest below.
			name: "Private Repository",
			payload: webhook.ParsedPayload{
				Ref:       "remote",
				CommitSHA: plumbing.NewHash(remoteFixtureCommit),
				Name:      "doco-cd_tests",
				FullName:  "kimdre/doco-cd_tests",
				CloneURL:  "https://github.com/kimdre/doco-cd_tests",
				Private:   true,
			},
			expectedStatusCode:   http.StatusCreated,
			expectedResponseBody: `{"content":"job completed successfully","job_id":"%[1]s"}`,
			customTarget:         "",
			swarmMode:            false,
		},
		{
			name: "Missing Deployment Configuration",
			payload: webhook.ParsedPayload{
				Ref:       git.MainBranch,
				CommitSHA: plumbing.NewHash("efefb4111f3c363692a2526f9be9b24560e6511f"),
				Name:      "kimdre",
				FullName:  "kimdre/kimdre",
				CloneURL:  "https://github.com/kimdre/kimdre",
				Private:   false,
			},
			expectedStatusCode:   http.StatusInternalServerError,
			expectedResponseBody: `{"error":"failed to get deploy configuration","content":"configuration file not found in repository: .doco-cd.y(a)ml","job_id":"%[1]s"}`,
			customTarget:         "",
			swarmMode:            false,
		},
		{
			name: "With Remote Repository",
			payload: webhook.ParsedPayload{
				Ref:       "remote",
				CommitSHA: plumbing.NewHash(remoteFixtureCommit),
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
				CommitSHA: plumbing.NewHash("01435dad4e7ff8f7da70202ca1ca77bccca9eb62"),
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

	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatalf("failed to get app config: %s", err.Error())
	}

	appConfig.GitCommitStatus = false

	// Route the localFixtureCloneURL used by the self-referencing subtests
	// above to the local fixture repos instead of a real network clone,
	// exercising the same webhook clone URL rewrite feature operators use to
	// mirror sources.
	appConfig.SourceURLRewrites = map[string]string{
		localFixtureCloneURL: selfDeployURL,
	}

	dockerCli, err := docker.CreateDockerCli(appConfig.DockerQuietDeploy)
	if err != nil {
		t.Fatalf("Failed to create Docker CLI: %v", err)
	}

	t.Cleanup(func() {
		if closeErr := dockerCli.Client().Close(); closeErr != nil {
			t.Logf("Failed to close Docker client: %v", closeErr)
		}
	})

	if err := swarm.RefreshModeEnabled(t.Context(), dockerCli.Client()); err != nil {
		log.Fatalf("Failed to check if Docker daemon is in Swarm mode: %v", err)
	}

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

			mode := swarm.GetModeEnabled()
			if mode != tc.swarmMode {
				t.Skipf("Skipping test because it requires swarm mode %v, but current mode is %v", tc.swarmMode, mode)
			}

			tmpDir := t.TempDir()

			stackName := test.ConvertTestName(t.Name())
			if len(stackName) > 40 {
				stackName = stackName[:40]
			}

			if tc.payload.Private && appConfig.GitAccessToken == "" {
				t.Skip("Skipping test for private repository because GIT_ACCESS_TOKEN is not set")
			}

			jobID := id.New()
			jobLog := logger.New(logger.LevelCritical).With(slog.String("job_id", jobID))

			ctx := context.Background()

			err = docker.VerifySocketConnection()
			if err != nil {
				t.Fatalf("Failed to verify docker socket connection: %v", err)
			}

			secretProvider, err := secretprovider.Initialize(ctx, appConfig.SecretProvider, "v0.0.0-test")
			if err != nil {
				if errors.Is(err, bitwardensecretsmanager.ErrNotSupported) {
					t.Skip(err.Error())
				}

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
				service, svcErr := compose.NewComposeService(dockerCli)

				downOpts := api.DownOptions{
					RemoveOrphans: true,
					Images:        "local",
					Volumes:       true,
				}

				if swarm.GetModeEnabled() {
					err = docker.RemoveSwarmStack(ctx, dockerCli, stackName)
				} else if svcErr == nil && service != nil {
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
			metadata := notification.Metadata{
				JobID:      jobID,
				Repository: git.GetRepoName(tc.payload.CloneURL),
				Revision:   notification.GetRevision(tc.payload.Ref, tc.payload.CommitSHAString()),
			}
			reconciliationManager := newTestReconciliationManager(t, reconciliation.Dependencies{
				AppConfig:      appConfig,
				DataMountPoint: testMountPoint,
				DockerCLI:      dockerCli,
				SecretProvider: secretProvider,
			})

			err = retry.New(
				retry.Attempts(3),
				retry.Delay(1*time.Second),
				retry.RetryIf(func(err error) bool {
					return strings.Contains(strings.ToLower(err.Error()), "no such image")
				}),
			).Do(func() error {
				if _, err := handleEvent(
					ctx,
					jobLog,
					rr,
					appConfig,
					testMountPoint,
					tc.payload,
					tc.customTarget,
					metadata,
					dockerCli,
					stackName,
					reconciliationManager,
				); err != nil {
					return err
				}

				expectedReturnMessage := fmt.Sprintf(tc.expectedResponseBody, jobID, filepath.Join(tmpDir, git.GetRepoName(tc.payload.CloneURL)), stackName) + "\n"
				if rr.Body.String() != expectedReturnMessage {
					return fmt.Errorf("handler returned unexpected body: got '%v' want '%v'",
						rr.Body.String(), expectedReturnMessage)
				}

				if status := rr.Code; status != tc.expectedStatusCode {
					return fmt.Errorf("handler returned wrong status code: got %v want %v",
						status, tc.expectedStatusCode)
				}

				return nil
			})
			if err != nil {
				t.Fatalf("test failed: %v", err)
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
		name         string
		source       string
		destination  string
		skipReadlink bool
		expectError  error
	}{
		{
			name:        "Valid Symlink Creation",
			source:      "source",
			destination: "destination",
			expectError: nil,
		},
		{
			name:         "same directory",
			source:       "same",
			destination:  "same",
			expectError:  nil,
			skipReadlink: true,
		},
		{
			name:        "end with slash",
			source:      "source1/",
			destination: "destination",
			expectError: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tmpDir := t.TempDir()

			source := filepath.Join(tmpDir, tc.source)
			destination := filepath.Join(tmpDir, tc.destination)

			err := CreateMountpointSymlink(container.MountPoint{
				Type:        "bind",
				Source:      source,
				Destination: destination,
				Mode:        "rw",
			})
			if !errors.Is(err, tc.expectError) {
				t.Errorf("symlink creation error: got %v, want %v", err, tc.expectError)
			}

			if tc.skipReadlink {
				return
			}

			link, err := os.Readlink(source)
			if err != nil {
				t.Errorf("failed to read symlink: %v", err)
			}

			if link != destination {
				t.Errorf("symlink destination: got %v, want %v", link, destination)
			}
		})
	}
}

func TestResolveDataMountPointUsesExplicitHostPath(t *testing.T) {
	detectionCalled := false

	mountPoint, err := resolveDataMountPoint(
		"/srv/doco-cd",
		"/data",
		func() (container.MountPoint, error) {
			detectionCalled = true

			return container.MountPoint{}, errors.New("mount point detection must not be called")
		},
	)
	if err != nil {
		t.Fatalf("expected explicit host path to resolve, got %v", err)
	}

	if detectionCalled {
		t.Fatal("expected explicit host path to bypass mount point detection")
	}

	expected := container.MountPoint{
		Type:        "bind",
		Source:      "/srv/doco-cd",
		Destination: "/data",
		Mode:        "rw",
		RW:          true,
	}
	if mountPoint != expected {
		t.Fatalf("expected mount point %+v, got %+v", expected, mountPoint)
	}
}

func TestResolveDataMountPointWithoutHostPath(t *testing.T) {
	expected := container.MountPoint{
		Type:        "volume",
		Source:      "/var/lib/docker/volumes/doco-cd/_data",
		Destination: "/data",
		Mode:        "rw",
		RW:          true,
	}
	testCases := []struct {
		name         string
		detectionErr error
	}{
		{name: "uses automatic detection"},
		{name: "returns detection error", detectionErr: errors.New("container unavailable on daemon")},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			detectionCalled := false

			mountPoint, err := resolveDataMountPoint(
				"",
				"/data",
				func() (container.MountPoint, error) {
					detectionCalled = true

					return expected, testCase.detectionErr
				},
			)
			if !errors.Is(err, testCase.detectionErr) {
				t.Fatalf("expected error %v, got %v", testCase.detectionErr, err)
			}

			if !detectionCalled {
				t.Fatal("expected empty host path to use automatic mount point detection")
			}

			if testCase.detectionErr == nil && mountPoint != expected {
				t.Fatalf("expected mount point %+v, got %+v", expected, mountPoint)
			}
		})
	}
}

func TestDetectDataMountPoint(t *testing.T) {
	const containerID = "doco-cd-container"

	expectedMountPoint := container.MountPoint{
		Source:      "/srv/doco-cd-data",
		Destination: "/data",
		RW:          true,
	}
	lookupErr := errors.New("container ID unavailable")
	inspectionErr := errors.New("container unavailable on daemon")
	testCases := []struct {
		name               string
		lookupErr          error
		inspectionErr      error
		expectedErr        error
		expectedErrMessage string
	}{
		{name: "returns detected mount point"},
		{
			name:               "wraps container lookup error",
			lookupErr:          lookupErr,
			expectedErr:        lookupErr,
			expectedErrMessage: "failed to retrieve doco-cd container id",
		},
		{
			name:               "wraps mount inspection error",
			inspectionErr:      inspectionErr,
			expectedErr:        inspectionErr,
			expectedErrMessage: "failed to retrieve /data mount point for container doco-cd-container",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			mountPoint, err := detectDataMountPoint(
				"/data",
				func() (string, error) {
					return containerID, testCase.lookupErr
				},
				func(gotContainerID, destination string) (container.MountPoint, error) {
					if gotContainerID != containerID {
						t.Fatalf("expected container ID %q, got %q", containerID, gotContainerID)
					}

					if destination != "/data" {
						t.Fatalf("expected destination %q, got %q", "/data", destination)
					}

					return expectedMountPoint, testCase.inspectionErr
				},
			)
			if !errors.Is(err, testCase.expectedErr) {
				t.Fatalf("expected error %v, got %v", testCase.expectedErr, err)
			}

			if testCase.expectedErrMessage != "" && !strings.Contains(err.Error(), testCase.expectedErrMessage) {
				t.Fatalf("expected error to contain %q, got %v", testCase.expectedErrMessage, err)
			}

			if testCase.expectedErr == nil && mountPoint != expectedMountPoint {
				t.Fatalf("expected mount point %+v, got %+v", expectedMountPoint, mountPoint)
			}
		})
	}
}
