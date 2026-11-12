package stages

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

func TestActiveHandoverSkipsPendingCommitStatus(t *testing.T) {
	previous := docker.SelfUpdateConfig()
	t.Cleanup(func() { docker.ConfigureSelfUpdate(previous) })

	tests := []struct {
		name      string
		stack     string
		wantPost  bool
		wantError error
	}{
		{name: "this stack", stack: "tools", wantError: selfupdate.ErrHandover},
		{name: "no record", wantPost: true},
		{name: "other stack", stack: "other", wantPost: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				mu    sync.Mutex
				posts []string
			)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read status body: %v", err)
				}

				mu.Lock()
				posts = append(posts, string(body))
				mu.Unlock()

				w.WriteHeader(http.StatusCreated)
			}))
			t.Cleanup(server.Close)

			store := selfupdate.NewStore(t.TempDir())
			docker.ConfigureSelfUpdate(docker.SelfUpdateOptions{Enabled: true, Store: store})

			if tt.stack != "" {
				if err := store.Create(&selfupdate.Record{
					ID:       "5056a901-3c03-4478-91d4-7a20aab30e8e",
					State:    selfupdate.StateStarted,
					Strategy: selfupdate.StrategyScaleOut,
					Stack:    tt.stack,
					Service:  "doco-cd",
				}); err != nil {
					t.Fatalf("create handover: %v", err)
				}
			}

			deployed := false
			sm := newTestStageManager(t)
			sm.JobTrigger = JobTriggerPoll
			sm.DeployConfig.Name = "tools"
			sm.AppConfig = &app.Config{
				GitCommitStatus: true,
				GitScmProvider:  "github",
				GitScmApiUrl:    config.HttpUrl(server.URL),
			}
			sm.Repository.SourceUrl = "https://github.com/owner/repo"
			sm.Repository.Revision = "deadbeef"

			err := sm.runStageOrder(t.Context(), nil, StageOrder{
				Order: []StageName{StagePreDeploy, StageDeploy},
				Funcs: map[StageName]StageFunc{
					StagePreDeploy: func(context.Context, *slog.Logger) error { return nil },
					StageDeploy: func(context.Context, *slog.Logger) error {
						deployed = true

						return nil
					},
				},
			})

			if !errors.Is(err, tt.wantError) {
				t.Fatalf("runStageOrder() error = %v, want %v", err, tt.wantError)
			}

			if deployed != (tt.wantError == nil) {
				t.Fatalf("deploy stage called = %v", deployed)
			}

			mu.Lock()
			got := append([]string(nil), posts...)
			mu.Unlock()

			body := strings.Join(got, "\n")
			posted := strings.Contains(body, "In Progress")

			if posted != tt.wantPost || (!tt.wantPost && len(got) != 0) {
				t.Fatalf("in progress posted = %v, requests = %d, bodies = %q", posted, len(got), body)
			}
		})
	}
}
