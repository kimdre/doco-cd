package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/controlplane"

	"github.com/kimdre/doco-cd/internal/test"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	restAPI "github.com/kimdre/doco-cd/internal/restapi"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

func TestHandler_TriggerPollHandler(t *testing.T) {
	// This is a placeholder test to ensure the TriggerPollHandler is registered and responds to requests.
	// Implementing a full test would require setting up a mock PollManager and verifying it receives the trigger call.
	testCases := []struct {
		name             string
		payload          *strings.Reader
		wait             bool
		expectedStatus   int
		expectedResponse string
	}{
		{
			name:             "With wait",
			payload:          strings.NewReader(`[{"url": "https://github.com/kimdre/doco-cd_tests.git", "reference": "main"}]`),
			wait:             true,
			expectedStatus:   http.StatusOK,
			expectedResponse: `{"content":"poll jobs complete","job_id":"[a-f0-9-]{36}"}`,
		},
		{
			name:             "Without wait",
			payload:          strings.NewReader(`[{"url": "https://github.com/kimdre/doco-cd_tests.git", "reference": "main"}]`),
			wait:             false,
			expectedStatus:   http.StatusAccepted,
			expectedResponse: `{"content":"poll jobs started","job_id":"[a-f0-9-]{36}"}`,
		},
		{
			name:             "With deploy config",
			payload:          strings.NewReader(`[{"url": "https://github.com/kimdre/doco-cd_tests.git", "reference": "main", "deployments": [{"name": "with-deploy-config"}]}]`),
			wait:             true,
			expectedStatus:   http.StatusOK,
			expectedResponse: `{"content":"poll jobs complete","job_id":"[a-f0-9-]{36}"}`,
		},
		{
			name:             "Empty body",
			payload:          strings.NewReader(``),
			wait:             false,
			expectedStatus:   http.StatusBadRequest,
			expectedResponse: `{"error":"failed to decode json in body","content":"EOF","job_id":"[a-f0-9-]{36}"}`,
		},
		{
			name:             "Invalid JSON",
			payload:          strings.NewReader(`[{"url": "https://github.com/kimdre/doco-cd_tests.git", "reference": "main"]`),
			wait:             false,
			expectedStatus:   http.StatusBadRequest,
			expectedResponse: `{"error":"failed to decode json in body","content":"invalid character ']' after object key:value pair","job_id":"[a-f0-9-]{36}"}`,
		},
	}

	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	appConfig.ApiSecret = "test-api-secret"

	dockerCli, err := docker.CreateDockerCli(appConfig.DockerQuietDeploy)
	if err != nil {
		t.Fatalf("Failed to create docker client: %v", err)
	}

	backend, err := compose.NewComposeService(dockerCli)
	if err != nil {
		t.Fatalf("Failed to create compose service: %v", err)
	}

	t.Cleanup(func() {
		err = dockerCli.Client().Close()
		if err != nil {
			return
		}
	})

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			log := logger.New(logger.LevelCritical)
			h := Handler{
				dockerCli: dockerCli,
				appConfig: appConfig,
				log:       log,

				controlPlaneRuns: newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
					appConfig: appConfig,
					dockerCli: dockerCli,
					log:       log,
					pollRunner: func(context.Context, poll.Config, *app.Config, container.MountPoint,
						command.Cli, *docker.ContextRegistry, *slog.Logger, notification.Metadata, *secretprovider.SecretProvider, string,
					) error {
						return nil
					},
				}),
			}

			endpoint := path.Join(APIPath, "/poll/run")

			rr := httptest.NewRecorder()

			mux := http.NewServeMux()
			mux.HandleFunc(endpoint, h.TriggerPollHandler)

			reqUrl := endpoint
			if tc.wait {
				reqUrl += "?wait=true"
			} else {
				reqUrl += "?wait=false"
			}

			req, err := http.NewRequest("POST", reqUrl, tc.payload)
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() {
				downOpts := api.DownOptions{
					RemoveOrphans: true,
					Images:        "local",
					Volumes:       true,
				}

				err = backend.Down(context.Background(), test.ConvertTestName(t.Name()), downOpts)
				if err != nil {
					t.Fatalf("Failed to remove test stack: %v", err)
				}
			})

			// Set headers
			req.Header.Set(restAPI.KeyHeader, appConfig.ApiSecret)
			mux.ServeHTTP(rr, req)

			if status := rr.Code; status != tc.expectedStatus {
				t.Errorf("handler returned wrong status code: got %v want %v", status, tc.expectedStatus)
			}

			regex, err := regexp.Compile(tc.expectedResponse)
			if err != nil {
				t.Fatal(err)
			}

			if !regex.MatchString(rr.Body.String()) {
				t.Errorf("handler returned unexpected body: got %v want %v", rr.Body.String(), tc.expectedResponse)
			}
		})
	}
}

func TestHandler_TriggerPollHandlerWithoutWait_DetachesRequestContext(t *testing.T) {
	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	appConfig.ApiSecret = "test-api-secret"

	ctxCancelled := make(chan bool, 1)

	log := logger.New(logger.LevelCritical)
	h := Handler{
		appConfig: appConfig,
		log:       log,

		controlPlaneRuns: newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
			appConfig: appConfig,
			log:       log,
			pollRunner: func(ctx context.Context, _ poll.Config, _ *app.Config, _ container.MountPoint,
				_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ *secretprovider.SecretProvider,
				_ string,
			) error {
				time.Sleep(50 * time.Millisecond)

				select {
				case <-ctx.Done():
					ctxCancelled <- true
				default:
					ctxCancelled <- false
				}

				return nil
			},
		}),
	}

	endpoint := path.Join(APIPath, "/poll/run")

	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc(endpoint, h.TriggerPollHandler)

	req, err := http.NewRequest("POST", endpoint+"?wait=false", strings.NewReader(`[{"url":"https://github.com/kimdre/doco-cd_tests.git","reference":"main"}]`))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set(restAPI.KeyHeader, appConfig.ApiSecret)
	mux.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusAccepted {
		t.Fatalf("handler returned wrong status code: got %v want %v", status, http.StatusAccepted)
	}

	regex := regexp.MustCompile(`{"content":"poll jobs started","job_id":"[a-f0-9-]{36}"}`)
	if !regex.MatchString(rr.Body.String()) {
		t.Fatalf("handler returned unexpected body: got %v", rr.Body.String())
	}

	select {
	case cancelled := <-ctxCancelled:
		if cancelled {
			t.Fatal("poll run context was cancelled after async API response")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for poll run")
	}
}

func TestHandler_TriggerPollHandlerRejectsInvalidRequestsBeforeTracking(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		query       string
		body        string
		maxPayload  int64
		wantStatus  int
		wantError   string
		wantContent string
	}{
		{name: "malformed wait", query: "?wait=eventually", body: `[{"url":"` + validPollSourceURL + `"}]`, maxPayload: 1024, wantStatus: http.StatusBadRequest},
		{name: "invalid JSON", body: `[{`, maxPayload: 1024, wantStatus: http.StatusBadRequest},
		{name: "invalid config", body: `[{}]`, maxPayload: 1024, wantStatus: http.StatusBadRequest, wantError: "invalid poll configuration at index 0", wantContent: "url"},
		{name: "empty config list", body: `[]`, maxPayload: 1024, wantStatus: http.StatusBadRequest, wantError: "no poll configuration provided in request body"},
		{name: "second JSON value", body: `[{"url":"` + validPollSourceURL + `"}] {}`, maxPayload: 1024, wantStatus: http.StatusBadRequest},
		{name: "trailing non-whitespace", body: `[{"url":"` + validPollSourceURL + `"}] trailing`, maxPayload: 1024, wantStatus: http.StatusBadRequest},
		{name: "oversized body", body: `[{"url":"` + validPollSourceURL + `"}]`, maxPayload: 8, wantStatus: http.StatusRequestEntityTooLarge},
		{name: "oversized valid prefix with trailing whitespace", body: `[{"url":"` + validPollSourceURL + `"}]  `, maxPayload: int64(len(`[{"url":"`+validPollSourceURL+`"}]`) + 1), wantStatus: http.StatusRequestEntityTooLarge},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			runs := 0
			log := logger.New(logger.LevelCritical)
			h := &Handler{
				appConfig: &app.Config{ApiSecret: "poll-secret", MaxPayloadSize: testCase.maxPayload}, // #nosec G101 -- test fixture.
				log:       log,
			}
			h.controlPlaneRuns = newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
				appConfig: h.appConfig,
				log:       log,
				pollRunner: func(context.Context, poll.Config, *app.Config, container.MountPoint,
					command.Cli, *docker.ContextRegistry, *slog.Logger, notification.Metadata, *secretprovider.SecretProvider, string,
				) error {
					runs++

					return nil
				},
			})
			body := &trackingReadCloser{Reader: strings.NewReader(testCase.body)}
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, APIPath+"/poll/run"+testCase.query, body)
			request.Header.Set(restAPI.KeyHeader, h.appConfig.ApiSecret)

			h.TriggerPollHandler(recorder, request)

			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, testCase.wantStatus, recorder.Body.String())
			}

			if testCase.wantError != "" {
				var response restAPI.ErrorResponse
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}

				if response.Error != testCase.wantError {
					t.Fatalf("error = %q, want %q", response.Error, testCase.wantError)
				}

				if testCase.wantContent != "" {
					content, ok := response.Content.(string)
					if !ok || !strings.Contains(content, testCase.wantContent) {
						t.Fatalf("content = %#v, want string containing %q", response.Content, testCase.wantContent)
					}
				}
			}

			if !body.closed {
				t.Fatal("request body was not closed")
			}

			if runs != 0 {
				t.Fatalf("invalid request started %d poll runs", runs)
			}

			if got := h.controlPlaneRuns.List(10, string(controlplane.RunTriggerPoll), ""); len(got) != 0 {
				t.Fatalf("invalid request was tracked: %#v", got)
			}
		})
	}
}

func TestHandler_TriggerPollHandlerAcceptsTrailingWhitespace(t *testing.T) {
	runs := 0
	log := logger.New(logger.LevelCritical)
	h := &Handler{
		appConfig: &app.Config{ApiSecret: "poll-secret", MaxPayloadSize: 1024}, // #nosec G101 -- test fixture.
		log:       log,
	}
	h.controlPlaneRuns = newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		appConfig: h.appConfig,
		log:       log,
		pollRunner: func(context.Context, poll.Config, *app.Config, container.MountPoint,
			command.Cli, *docker.ContextRegistry, *slog.Logger, notification.Metadata, *secretprovider.SecretProvider, string,
		) error {
			runs++

			return nil
		},
	})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, APIPath+"/poll/run", strings.NewReader(`[{"url":"`+validPollSourceURL+`"}] `+"\n\t"))
	request.Header.Set(restAPI.KeyHeader, h.appConfig.ApiSecret)

	h.TriggerPollHandler(response, request)

	if response.Code != http.StatusOK || runs != 1 {
		t.Fatalf("status = %d, runs = %d, body = %s", response.Code, runs, response.Body.String())
	}
}

func TestHandler_TriggerPollHandlerReportsPollFailures(t *testing.T) {
	log := logger.New(logger.LevelCritical)
	h := &Handler{
		appConfig: &app.Config{ApiSecret: "poll-secret", MaxPayloadSize: 1024}, // #nosec G101 -- test fixture.
		log:       log,
	}
	h.controlPlaneRuns = newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		appConfig: h.appConfig,
		log:       log,
		pollRunner: func(_ context.Context, cfg poll.Config, _ *app.Config, _ container.MountPoint,
			_ command.Cli, _ *docker.ContextRegistry, _ *slog.Logger, _ notification.Metadata, _ *secretprovider.SecretProvider, _ string,
		) error {
			if strings.Contains(cfg.SourceUrl, "failed") {
				return errors.New("poll failed")
			}

			return nil
		},
	})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, APIPath+"/poll/run", strings.NewReader(`[
		{"url":"https://example.com/succeeded.git"},
		{"url":"https://example.com/failed.git"}
	]`))
	request.Header.Set(restAPI.KeyHeader, h.appConfig.ApiSecret)

	h.TriggerPollHandler(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusInternalServerError, response.Body.String())
	}

	var result restAPI.ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}

	if result.Error != "1/2 poll jobs failed" || result.JobID == "" {
		t.Fatalf("response = %#v", result)
	}

	run, ok := h.controlPlaneRuns.Get(result.JobID)
	if !ok || run.Status != controlplane.RunStatusFailed || run.Message != "1/2 poll jobs failed" {
		t.Fatalf("tracked run = %#v, found = %t", run, ok)
	}
}

func TestHandler_TriggerPollHandlerRejectsAsyncWorkDuringShutdown(t *testing.T) {
	log := logger.New(logger.LevelCritical)
	h := &Handler{
		appConfig: &app.Config{ApiSecret: "poll-secret", MaxPayloadSize: 1024}, // #nosec G101 -- test fixture.
		log:       log,
	}
	h.controlPlaneRuns = newTestControlPlaneRuns(t, testControlPlaneRunsOptions{
		appConfig: h.appConfig,
		closed:    true,
		log:       log,
	})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, APIPath+"/poll/run?wait=false", strings.NewReader(`[{"url":"`+validPollSourceURL+`"}]`))
	request.Header.Set(restAPI.KeyHeader, h.appConfig.ApiSecret)

	h.TriggerPollHandler(response, request)

	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), controlplane.ErrBackgroundWorkClosed.Error()) {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
}

type trackingReadCloser struct {
	io.Reader
	closed bool
}

func (r *trackingReadCloser) Close() error {
	r.closed = true

	return nil
}
