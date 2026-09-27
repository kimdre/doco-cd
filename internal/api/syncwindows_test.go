package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/logger"
	restAPI "github.com/kimdre/doco-cd/internal/restapi"
	"github.com/kimdre/doco-cd/internal/syncwindow"
)

func TestHandler_GetSyncWindowsHandler(t *testing.T) {
	t.Parallel()

	appConfig, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	appConfig.ApiSecret = "test-api-secret"

	// A deny window that is always active: it starts every minute and lasts
	// two minutes.
	policy, err := syncwindow.New([]syncwindow.Window{{
		Name:         "freeze",
		Kind:         syncwindow.KindDeny,
		Schedule:     "* * * * *",
		Duration:     2 * time.Minute,
		Repositories: []string{"github.com/acme/*"},
		ManualSync:   true,
	}})
	if err != nil {
		t.Fatal(err)
	}

	appConfig.SyncWindows = policy

	h := Handler{appConfig: appConfig, log: logger.New(logger.LevelCritical)}
	endpoint := APIPath + "/sync-windows"

	tests := []struct {
		name         string
		method       string
		query        string
		noAPIKey     bool
		wantStatus   int
		wantDecision *syncwindow.Decision
	}{
		{name: "invalid method", method: http.MethodPost, wantStatus: http.StatusMethodNotAllowed},
		{name: "missing api key", method: http.MethodGet, noAPIKey: true, wantStatus: http.StatusUnauthorized},
		{name: "invalid origin", method: http.MethodGet, query: "?origin=reconciliation", wantStatus: http.StatusBadRequest},
		{name: "windows only", method: http.MethodGet, wantStatus: http.StatusOK},
		{
			name:         "blocked target",
			method:       http.MethodGet,
			query:        "?repository=github.com/acme/app&deployment=web",
			wantStatus:   http.StatusOK,
			wantDecision: &syncwindow.Decision{Windows: []string{"freeze"}},
		},
		{
			name:         "manual target",
			method:       http.MethodGet,
			query:        "?repository=github.com/acme/app&origin=Manual",
			wantStatus:   http.StatusOK,
			wantDecision: &syncwindow.Decision{Allowed: true, ManualOverride: true, Windows: []string{"freeze"}},
		},
		{
			name:         "unmatched target",
			method:       http.MethodGet,
			query:        "?repository=github.com/other/app",
			wantStatus:   http.StatusOK,
			wantDecision: &syncwindow.Decision{Allowed: true, Windows: []string{}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(tc.method, endpoint+tc.query, nil)
			if !tc.noAPIKey {
				req.Header.Set(restAPI.KeyHeader, appConfig.ApiSecret)
			}

			rr := httptest.NewRecorder()
			h.GetSyncWindowsHandler(rr, req)

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}

			if tc.wantStatus != http.StatusOK {
				return
			}

			var body successEnvelope[syncwindow.Report]
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}

			report := body.Content
			if len(report.Windows) != 1 || report.Windows[0].Name != "freeze" || !report.Windows[0].Active {
				t.Fatalf("windows = %+v", report.Windows)
			}

			if tc.wantDecision == nil {
				if report.Decision != nil {
					t.Fatalf("decision = %+v, want none", report.Decision)
				}

				return
			}

			got := report.Decision
			if got == nil || got.Allowed != tc.wantDecision.Allowed || got.ManualOverride != tc.wantDecision.ManualOverride ||
				len(got.Windows) != len(tc.wantDecision.Windows) || got.Windows == nil {
				t.Fatalf("decision = %+v, want %+v", got, tc.wantDecision)
			}
		})
	}
}
