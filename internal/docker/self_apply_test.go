package docker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

type selfApplyTestClient struct {
	client.APIClient
	started  int
	removed  int
	stopped  bool
	startErr error
}

// ContainerInspect returns the mocked predecessor state for applier tests.
func (c *selfApplyTestClient) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	previous := applierSourceInspect()
	previous.State = &container.State{Running: !c.stopped}

	return client.ContainerInspectResult{Container: previous}, nil
}

// ContainerStart records attempts to restore a stopped predecessor.
func (c *selfApplyTestClient) ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error) {
	c.started++
	if c.startErr != nil {
		return client.ContainerStartResult{}, c.startErr
	}

	c.stopped = false

	return client.ContainerStartResult{}, nil
}

// ContainerList returns the replacement containers configured by the test.
func (c *selfApplyTestClient) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	return client.ContainerListResult{}, nil
}

// ContainerRemove records cleanup of the applier or replacement container.
func (c *selfApplyTestClient) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	c.removed++
	return client.ContainerRemoveResult{}, nil
}

type selfApplyTestCli struct {
	command.Cli
	apiClient client.APIClient
}

// Client supplies the mocked Docker API to the applier.
func (c selfApplyTestCli) Client() client.APIClient { return c.apiClient }

// TestApplySelfUpdatePreflightFailureKeepsPredecessor checks that errors
// before readiness leave the running container in service.
func TestApplySelfUpdatePreflightFailureKeepsPredecessor(t *testing.T) {
	repoURL := "https://example.com/owner/repo"

	for _, tc := range []struct {
		name    string
		want    string
		prepare func(t *testing.T, root, repoPath, workDir string, labels map[string]string)
	}{
		{
			name: "missing compose reference",
			want: "rebuild the compose reference",
			prepare: func(_ *testing.T, _, _, _ string, labels map[string]string) {
				delete(labels, api.ProjectLabel)
			},
		},
		{
			name: "source lock unavailable",
			want: "lock the cached source",
			prepare: func(t *testing.T, _, repoPath, _ string, _ map[string]string) {
				t.Helper()

				if err := os.MkdirAll(repoPath+".gc-use.lock", 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:    "source reload unavailable",
			want:    "load the self stack",
			prepare: func(*testing.T, string, string, string, map[string]string) {},
		},
		{
			name: "self service missing from reloaded project",
			want: "select the self service",
			prepare: func(t *testing.T, _, repoPath, workDir string, _ map[string]string) {
				t.Helper()

				if err := os.MkdirAll(repoPath, 0o700); err != nil {
					t.Fatal(err)
				}

				for name, content := range map[string]string{
					".doco-cd.yml": "name: self-stack\nreference: refs/heads/main\nworking_dir: .\ncompose_files:\n  - compose.yaml\n",
					"compose.yaml": "services:\n  other:\n    image: alpine:3.22\n",
				} {
					if err := os.WriteFile(filepath.Join(workDir, name), []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			repoPath := filepath.Join(root, git.GetRepoName(repoURL))
			workDir := repoPath
			labels := map[string]string{
				api.ProjectLabel:                  "self-stack",
				api.ServiceLabel:                  "app",
				api.WorkingDirLabel:               workDir,
				api.ConfigFilesLabel:              filepath.Join(workDir, "compose.yaml"),
				DocoCDLabels.Source.URL:           repoURL,
				DocoCDLabels.Source.Type:          "git",
				DocoCDLabels.Deployment.Name:      "self-stack",
				DocoCDLabels.Deployment.TargetRef: "refs/heads/main",
			}
			tc.prepare(t, root, repoPath, workDir, labels)

			store := selfupdate.NewStore(root)

			record := selfupdate.Record{
				ID: "preflight", State: selfupdate.StateApplying,
				Stack: "self-stack", Service: "app",
				Predecessor: selfupdate.ContainerRef{ID: "old", Name: "self-stack-app-1"},
				Labels:      labels,
			}
			if err := store.Create(&record); err != nil {
				t.Fatal(err)
			}

			fake := &selfApplyTestClient{}

			err := ApplySelfUpdate(t.Context(), selfApplyTestCli{apiClient: fake}, ApplySelfOptions{
				Store: store, JournalID: record.ID, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
				Scheduled: ScheduledComposeOptions{ComposeLoad: ComposeLoadOptions{DataMountPath: root}, DeployConfigBaseDir: "/"},
			})
			if err != nil {
				t.Fatalf("ApplySelfUpdate() error = %v; want terminal failed journal", err)
			}

			after, err := store.Load(record.ID)
			if err != nil {
				t.Fatal(err)
			}

			if after.State != selfupdate.StateFailed || !strings.Contains(after.Error, tc.want) {
				t.Errorf("journal state/reason = %s/%q; want failed with %q", after.State, after.Error, tc.want)
			}

			for _, transition := range after.History {
				if transition.State == selfupdate.StateApplyReady || transition.State == selfupdate.StateApplyDrained {
					t.Errorf("preflight failure incorrectly requested predecessor drain: %+v", after.History)
				}
			}

			if after.Restored.ID != record.Predecessor.ID || fake.started != 0 || fake.removed != 0 {
				t.Errorf("predecessor disturbed: restored=%+v starts=%d removes=%d", after.Restored, fake.started, fake.removed)
			}
		})
	}
}

// TestFailSelfUpdateBeforeApplyRestoresStoppedPredecessor checks recovery
// when setup fails after the predecessor was already stopped.
func TestFailSelfUpdateBeforeApplyRestoresStoppedPredecessor(t *testing.T) {
	store := selfupdate.NewStore(t.TempDir())

	record := selfupdate.Record{
		ID: "early-error", State: selfupdate.StateApplying, Stack: "self-stack", Service: "app",
		Predecessor: selfupdate.ContainerRef{ID: "old", Name: "self-stack-app-1"},
	}
	if err := store.Create(&record); err != nil {
		t.Fatal(err)
	}

	fake := &selfApplyTestClient{stopped: true, startErr: errors.New("daemon temporarily unavailable")}
	cause := errors.New("initialize secret provider: credentials unavailable")
	cli := selfApplyTestCli{apiClient: fake}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := FailSelfUpdate(t.Context(), cli, store, record.ID, cause, log)
	if err == nil || !strings.Contains(err.Error(), "daemon temporarily unavailable") {
		t.Fatalf("incomplete recovery error = %v", err)
	}

	pending, err := store.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}

	if pending.State != selfupdate.StateApplying ||
		!strings.Contains(pending.Error, cause.Error()) ||
		!strings.Contains(pending.Error, "restore also failed") {
		t.Errorf("pending journal = %s/%q; want applying with both errors", pending.State, pending.Error)
	}

	fake.startErr = nil

	if err = FailSelfUpdate(t.Context(), cli, store, record.ID, cause, log); err != nil {
		t.Fatalf("retry early failure recovery: %v", err)
	}

	terminal, err := store.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}

	if terminal.State != selfupdate.StateRolledBack || terminal.Restored.ID != record.Predecessor.ID ||
		terminal.Error != cause.Error() || fake.stopped {
		t.Errorf("recovery = %+v, predecessor stopped=%v; want rolled back and running", terminal, fake.stopped)
	}

	if err = FailSelfUpdate(t.Context(), cli, store, record.ID, cause, log); err != nil {
		t.Fatalf("terminal recovery must not repeat: %v", err)
	}

	if fake.started != 2 || fake.removed != 0 {
		t.Errorf("recovery start/remove calls = %d/%d, want 2/0", fake.started, fake.removed)
	}
}

// TestFailSelfUpdateRestoresOnlyStoppedPredecessor checks the recovery
// outcome for each applier phase: a running predecessor is left untouched and
// the update fails, a stopped one is restarted and the update rolls back.
func TestFailSelfUpdateRestoresOnlyStoppedPredecessor(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     selfupdate.State
		stopped   bool
		wantState selfupdate.State
	}{
		{name: "applying running", state: selfupdate.StateApplying, wantState: selfupdate.StateFailed},
		{name: "applying stopped", state: selfupdate.StateApplying, stopped: true, wantState: selfupdate.StateRolledBack},
		{name: "ready stopped", state: selfupdate.StateApplyReady, stopped: true, wantState: selfupdate.StateRolledBack},
		{name: "drained stopped", state: selfupdate.StateApplyDrained, stopped: true, wantState: selfupdate.StateRolledBack},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := selfupdate.NewStore(t.TempDir())

			record := selfupdate.Record{
				ID: "early-error", State: tc.state, Stack: "self-stack", Service: "app",
				Predecessor: selfupdate.ContainerRef{ID: "old", Name: "self-stack-app-1"},
			}
			if err := store.Create(&record); err != nil {
				t.Fatal(err)
			}

			fake := &selfApplyTestClient{stopped: tc.stopped}

			cause := errors.New("resolve data mount: unavailable")
			if err := FailSelfUpdate(t.Context(), selfApplyTestCli{apiClient: fake}, store, record.ID, cause, nil); err != nil {
				t.Fatalf("recover failed applier: %v", err)
			}

			terminal, err := store.Load(record.ID)
			if err != nil {
				t.Fatal(err)
			}

			wantStarts := 0
			if tc.stopped {
				wantStarts = 1
			}

			if terminal.State != tc.wantState || terminal.Restored.ID != record.Predecessor.ID ||
				terminal.Error != cause.Error() || fake.stopped || fake.started != wantStarts || fake.removed != 0 {
				t.Errorf("journal = %+v, predecessor stopped/starts/removes=%v/%d/%d; want %s with %d starts",
					terminal, fake.stopped, fake.started, fake.removed, tc.wantState, wantStarts)
			}
		})
	}
}
