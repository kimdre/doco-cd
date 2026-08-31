package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kimdre/doco-cd/internal/logger"
)

func TestDockerCliForRequestContextValidation(t *testing.T) {
	t.Parallel()

	h := handlerData{log: logger.New(logger.LevelCritical)}
	jobLog := h.log.With()

	tests := []struct {
		name       string
		url        string
		wantOK     bool
		wantStatus int
		wantHeader string
	}{
		{"default omitted", "/v1/api/projects", true, http.StatusOK, "default"},
		{"default explicit", "/v1/api/projects?context=default", true, http.StatusOK, "default"},
		{"unknown named context", "/v1/api/projects?context=remote", false, http.StatusBadRequest, "remote"},
		{"repeated context", "/v1/api/projects?context=default&context=remote", false, http.StatusBadRequest, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			recorder := httptest.NewRecorder()

			_, _, ok := h.dockerCliForRequest(recorder, req, jobLog, "job-id")
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}

			status := recorder.Code
			if tt.wantOK {
				status = http.StatusOK
			}

			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d", status, tt.wantStatus)
			}

			if got := recorder.Header().Get(dockerContextHeader); got != tt.wantHeader {
				t.Fatalf("%s = %q, want %q", dockerContextHeader, got, tt.wantHeader)
			}
		})
	}
}
