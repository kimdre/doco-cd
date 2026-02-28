package prometheus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/kimdre/doco-cd/internal/config"
)

// TestServe tests the metrics endpoint serving functionality.
func TestServe(t *testing.T) {
	t.Parallel()

	expectedStatusCode := 200
	expectedContentType := "text/plain; version=0.0.4; charset=utf-8; escaping=underscores"

	appConfig, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get app config: %v", err)
	}

	AppInfo.WithLabelValues("test", appConfig.LogLevel, time.Now().Format(time.RFC3339)).Set(1)

	req, err := http.NewRequest("GET", MetricsPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()

	handler := promhttp.Handler()
	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != expectedStatusCode {
		t.Errorf("Expected status code %d, got %d", expectedStatusCode, status)
	}

	if contentType := rr.Header().Get("Content-Type"); contentType != expectedContentType {
		t.Errorf("Expected Content-Type %s, got %s", expectedContentType, contentType)
	}

	// Check if the response body is not empty
	if rr.Body.Len() == 0 {
		t.Error("Expected non-empty response body, got empty")
	}

	// Check if the response body contains the expected metrics
	if !strings.Contains(rr.Body.String(), "doco_cd_info") {
		t.Error("Expected response body to contain 'doco_cd_info' metric, but it does not")
	}
}
