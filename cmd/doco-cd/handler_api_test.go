package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
)

// Make http call to HealthCheckHandler.
func TestHandlerData_HealthCheckHandler(t *testing.T) {
	t.Parallel()

	expectedResponse := `{"content":"healthy","job_id":"[a-f0-9-]{36}"}`
	expectedStatusCode := http.StatusOK

	appConfig, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	log := logger.New(logger.LevelCritical)

	dockerCli, err := docker.CreateDockerCli(appConfig.DockerQuietDeploy, !appConfig.SkipTLSVerification)
	if err != nil {
		t.Fatalf("Failed to create docker client: %v", err)
	}

	t.Cleanup(func() {
		err = dockerCli.Client().Close()
		if err != nil {
			return
		}
	})

	h := handlerData{
		dockerCli:  dockerCli,
		appConfig:  appConfig,
		appVersion: config.AppVersion,
		log:        log,
	}

	req, err := http.NewRequest("GET", healthPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(h.HealthCheckHandler)
	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != expectedStatusCode {
		t.Errorf("handler returned wrong status code: got %v want %v", status, expectedStatusCode)
	}

	regex, err := regexp.Compile(expectedResponse)
	if err != nil {
		t.Fatal(err)
	}

	if !regex.MatchString(rr.Body.String()) {
		t.Errorf("handler returned unexpected body: got %v want %v", rr.Body.String(), expectedResponse)
	}
}
