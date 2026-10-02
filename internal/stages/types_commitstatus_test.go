package stages

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/selfupdate"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// newTestStageManagerForCommitStatus builds a minimal StageManager sufficient for exercising
// resolveCommitStatusRequest without going through NewStageManager.
func newTestStageManagerForCommitStatus(appConfig *app.Config, repoURL string) *StageManager {
	return &StageManager{
		Log:          slog.Default(),
		AppConfig:    appConfig,
		DeployConfig: &deploy.Config{Name: "stack"},
		Repository: &RepositoryData{
			SourceUrl: repoURL,
			Revision:  "deadbeef",
		},
	}
}

// TestResolveCommitStatusRequest_DelegatesToCommitStatusPackage is a thin wiring test:
// detailed credential-precedence and skip-rule behavior is covered directly against
// commitstatus.ResolveRequest in internal/commitstatus. This only verifies that the
// StageManager wires its own configuration/repository state through correctly.
func TestResolveCommitStatusRequest_DelegatesToCommitStatusPackage(t *testing.T) {
	gitInternal.ConfigureAuthResolver(nil, "", "", "pat-token", "", gitInternal.GitHubAppConfig{})
	t.Cleanup(func() {
		gitInternal.ConfigureAuthResolver(nil, "", "", "", "", gitInternal.GitHubAppConfig{})
	})

	sm := newTestStageManagerForCommitStatus(&app.Config{
		GitCommitStatus: true,
		GitScmProvider:  "github",
		GitAccessToken:  "pat-token",
	}, "https://github.com/org/repo.git")

	req, ok := sm.resolveCommitStatusRequest()
	if !ok {
		t.Fatal("expected resolveCommitStatusRequest to succeed")
	}

	if req.Token != "pat-token" {
		t.Fatalf("expected explicit access token, got '%s'", req.Token)
	}

	if req.Context != "doco-cd/stack" {
		t.Fatalf("expected context derived from deploy config, got %q", req.Context)
	}
}

func TestPostQueuedCommitStatusUsesDeploymentContext(t *testing.T) {
	gitInternal.ConfigureAuthResolver(nil, "", "", "pat-token", "", gitInternal.GitHubAppConfig{})
	t.Cleanup(func() {
		gitInternal.ConfigureAuthResolver(nil, "", "", "", "", gitInternal.GitHubAppConfig{})
	})

	var received map[string]string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode status request: %v", err)
		}

		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	sm := newTestStageManagerForCommitStatus(&app.Config{
		GitCommitStatus: true,
		GitScmProvider:  "gitea",
		GitScmApiUrl:    config.HttpUrl(server.URL),
		GitAccessToken:  "pat-token",
	}, server.URL+"/org/repo.git")
	sm.JobTrigger = JobTriggerWebhook

	sm.PostQueuedCommitStatus(t.Context())

	if received["state"] != "pending" || received["description"] != "Queued" {
		t.Fatalf("unexpected queued status: %v", received)
	}

	if received["context"] != "doco-cd/stack" {
		t.Fatalf("expected deployment-specific context, got %q", received["context"])
	}
}

func TestResolveCommitStatusRequest_SkipsWhenDisabled(t *testing.T) {
	sm := newTestStageManagerForCommitStatus(&app.Config{
		GitCommitStatus: false,
	}, "https://github.com/org/repo.git")

	_, ok := sm.resolveCommitStatusRequest()
	if ok {
		t.Fatal("expected resolveCommitStatusRequest to skip when commit statuses are disabled")
	}
}

func TestResolveCommitStatusRequest_SkipsForOCISource(t *testing.T) {
	sm := newTestStageManagerForCommitStatus(&app.Config{
		GitCommitStatus: true,
	}, "ghcr.io/org/artifact:latest")
	sm.Repository.Source = "oci"

	_, ok := sm.resolveCommitStatusRequest()
	if ok {
		t.Fatal("expected resolveCommitStatusRequest to skip for OCI sources")
	}
}

// TestSelfUpdateCommitStatusRecordsPendingTarget checks that a self-update
// hands over the same target that its pending status was posted to.
func TestSelfUpdateCommitStatusRecordsPendingTarget(t *testing.T) {
	startedAt := time.Date(2026, time.September, 27, 21, 0, 0, 0, time.UTC)

	sm := newTestStageManagerForCommitStatus(&app.Config{GitCommitStatus: true}, "https://git.example.com/org/deploy.git")
	sm.DeployConfig.Internal.ConfigTarget = "nas"
	sm.Stages = &Stages{Init: &InitStageData{StartedAt: startedAt}}
	sm.Payload = &webhook.ParsedPayload{WebURL: "https://git.example.com/org/config", FullName: "org/config"}

	got := sm.selfUpdateCommitStatus()
	if got == nil {
		t.Fatal("expected a commit status target")
	}

	want := selfupdate.CommitStatusInfo{
		SourceURL: "https://git.example.com/org/deploy.git",
		RepoURL:   "https://git.example.com/org/config",
		FullName:  "org/config",
		CommitSHA: "deadbeef",
		Context:   sm.resolveCommitStatusContext(),
		StartedAt: startedAt,
	}
	if *got != want {
		t.Fatalf("selfUpdateCommitStatus() = %+v, want %+v", *got, want)
	}

	if want.Context != "doco-cd/nas/stack" {
		t.Fatalf("context = %q, want the pending status context", want.Context)
	}
}

func TestSelfUpdateCommitStatusCopiesNativeTarget(t *testing.T) {
	t.Parallel()

	sm := newTestStageManagerForCommitStatus(&app.Config{GitCommitStatus: true}, "https://github.com/org/repo.git")
	sm.commitStatusTarget = &commitstatus.Target{
		Backend: commitstatus.BackendChecks, CheckRunID: 123, ExternalID: "attempt", AppID: "42",
	}

	info := sm.selfUpdateCommitStatus()
	if info == nil || info.Target == nil || *info.Target != *sm.commitStatusTarget {
		t.Fatalf("native check target not handed over: %+v", info)
	}

	if info.Target == sm.commitStatusTarget {
		t.Fatal("journal must snapshot the target rather than share mutable reporting state")
	}
}

func TestSelfUpdateCommitStatusSkipsWithoutPendingStatus(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*StageManager)
	}{
		{name: "disabled", setup: func(sm *StageManager) { sm.AppConfig.GitCommitStatus = false }},
		{name: "oci source", setup: func(sm *StageManager) { sm.Repository.Source = config.SourceTypeOCI }},
		{name: "destroy", setup: func(sm *StageManager) { sm.DeployConfig.Destroy.Enabled = true }},
		{name: "no commit sha", setup: func(sm *StageManager) { sm.Repository.Revision = "" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sm := newTestStageManagerForCommitStatus(&app.Config{GitCommitStatus: true}, "https://git.example.com/org/repo.git")
			tt.setup(sm)

			if got := sm.selfUpdateCommitStatus(); got != nil {
				t.Fatalf("selfUpdateCommitStatus() = %+v, want nil", *got)
			}
		})
	}
}

// TestCommitStatusUsesPublishedRevision checks that the commit status belongs to
// the revision this deployment published, not to the branch head a later push
// has since fetched into the shared mirror, and that a request revision resolved
// for another reference is not used for a stack before the init stage resolved
// the stack's own reference.
func TestCommitStatusUsesPublishedRevision(t *testing.T) {
	t.Parallel()

	originPath, mirrorPath := setupOriginAndMirror(t)

	resolve := func(reference string) string {
		t.Helper()

		sm := newMirrorStageManager(mirrorPath)
		sm.DeployConfig.Reference = reference

		sha, err := sm.latestCommitFromMirror()
		if err != nil {
			t.Fatalf("latestCommitFromMirror(%s) = %v", reference, err)
		}

		return sha
	}

	published := resolve("main")

	runGit(t, originPath, "checkout", "-b", "release")
	writeFile(t, filepath.Join(originPath, "README.md"), "release\n")
	runGit(t, originPath, "add", ".")
	runGit(t, originPath, "commit", "-m", "release commit")
	runGit(t, originPath, "checkout", "main")
	writeFile(t, filepath.Join(originPath, "README.md"), "second\n")
	runGit(t, originPath, "add", ".")
	runGit(t, originPath, "commit", "-m", "second commit")
	runGitBare(t, mirrorPath, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")

	head := resolve("main")
	release := resolve("release")

	if head == published {
		t.Fatal("mirror head did not move")
	}

	tests := []struct {
		name                 string
		disabled             bool
		resolvedReference    string
		revision             string
		resolvedOwnReference bool
		want                 string
	}{
		{
			name:              "request revision of the stack's reference",
			resolvedReference: "refs/heads/main",
			revision:          published,
			want:              published,
		},
		{
			name:                 "revision the init stage resolved for the stack's own reference",
			resolvedReference:    "refs/heads/release",
			revision:             published,
			resolvedOwnReference: true,
			want:                 published,
		},
		{
			// The init stage failed before resolving the stack's own reference.
			name:              "request revision of another reference",
			resolvedReference: "refs/heads/release",
			revision:          release,
			want:              head,
		},
		{
			name:              "no revision",
			resolvedReference: "refs/heads/main",
			want:              head,
		},
		{
			name:              "commit statuses disabled",
			disabled:          true,
			resolvedReference: "refs/heads/main",
			revision:          published,
			want:              "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sm := newMirrorStageManager(mirrorPath)
			sm.AppConfig = &app.Config{GitCommitStatus: !tc.disabled}
			sm.Repository.Source = config.SourceTypeGit
			sm.Repository.ResolvedReference = tc.resolvedReference
			sm.Repository.Revision = tc.revision
			sm.resolvedOwnReference = tc.resolvedOwnReference

			if got := sm.commitStatusParams().CommitSHA; got != tc.want {
				t.Fatalf("commit status SHA = %q, want %q", got, tc.want)
			}
		})
	}
}
