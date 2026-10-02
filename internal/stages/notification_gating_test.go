package stages

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/notification"
)

type sentNotification struct {
	level    notification.Level
	metadata notification.Metadata
}

// levelGatedSender delivers notifications at or above minLevel, like a Notifier
// configured with that notify level, and records every Send call.
type levelGatedSender struct {
	minLevel notification.Level

	mu   sync.Mutex
	sent []sentNotification
}

func (s *levelGatedSender) Enabled(level notification.Level) bool {
	return level >= s.minLevel
}

func (s *levelGatedSender) Send(level notification.Level, _, _ string, metadata notification.Metadata, _ ...notification.SendOption) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sent = append(s.sent, sentNotification{level: level, metadata: metadata})

	return nil
}

func (s *levelGatedSender) sentNotifications() []sentNotification {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]sentNotification(nil), s.sent...)
}

func newNotifyingStageManager(mirrorDir, revision string, sender notification.Sender) *StageManager {
	sm := &StageManager{
		Stages: &Stages{
			Init:       &InitStageData{MetaData: NewMetaData(StageInit)},
			PostDeploy: &PostDeployStageData{MetaData: NewMetaData(StagePostDeploy)},
		},
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		JobID:        "job-1",
		DeployConfig: &deploy.Config{Name: "app", Reference: "main"},
		DeployState:  &DeploymentState{},
		Docker:       &Docker{},
		Repository: &RepositoryData{
			Name:      "owner/repo",
			Source:    config.SourceTypeGit,
			MirrorDir: mirrorDir,
			Revision:  revision,
		},
		Notifier: sender,
	}
	sm.Stages.Init.StartedAt = time.Now()

	return sm
}

func mirrorHead(t *testing.T, mirrorPath string) string {
	t.Helper()

	head, err := newMirrorStageManager(mirrorPath).latestCommitFromMirror()
	if err != nil {
		t.Fatalf("latestCommitFromMirror() = %v", err)
	}

	return head
}

// TestNotifyDeploymentStartedSkipsCommitLookupBelowLevel ensures the "Deployment
// started" notification, which the default notify level drops, no longer reads the
// mirror to shorten the commit SHA.
func TestNotifyDeploymentStartedSkipsCommitLookupBelowLevel(t *testing.T) {
	t.Parallel()

	_, mirrorPath := setupOriginAndMirror(t)
	head := mirrorHead(t, mirrorPath)

	dropped := &levelGatedSender{minLevel: notification.Success}
	sm := newNotifyingStageManager(mirrorPath, head, dropped)

	if err := sm.NotifyDeploymentStarted(); err != nil {
		t.Fatalf("NotifyDeploymentStarted() = %v", err)
	}

	if sent := dropped.sentNotifications(); len(sent) != 0 {
		t.Fatalf("NotifyDeploymentStarted() sent %d notifications below the notify level, want 0", len(sent))
	}

	if _, ok := sm.cachedShortCommitSHA(head); ok {
		t.Fatal("NotifyDeploymentStarted() computed the short SHA for a notification it does not send")
	}

	delivered := &levelGatedSender{minLevel: notification.Info}
	sm.Notifier = delivered

	if err := sm.NotifyDeploymentStarted(); err != nil {
		t.Fatalf("NotifyDeploymentStarted() = %v", err)
	}

	sent := delivered.sentNotifications()
	if len(sent) != 1 || sent[0].level != notification.Info {
		t.Fatalf("NotifyDeploymentStarted() sent %+v, want one info notification", sent)
	}

	if want := notification.GetRevision("main", head[:7]); sent[0].metadata.Revision != want {
		t.Fatalf("notification revision = %q, want %q", sent[0].metadata.Revision, want)
	}
}

// TestPostDeploySkipsCommitDetailsBelowLevel ensures the post-deploy stage still
// sends the success notification, which clears the stack's reported failure, but
// skips the mirror reads that only decorate it when it is not delivered.
func TestPostDeploySkipsCommitDetailsBelowLevel(t *testing.T) {
	t.Parallel()

	revision := strings.Repeat("a", 40)
	// Reading this mirror fails, so the stage only succeeds if it skips the read.
	missingMirror := filepath.Join(t.TempDir(), "missing.git")

	dropped := &levelGatedSender{minLevel: notification.Failure}
	sm := newNotifyingStageManager(missingMirror, revision, dropped)

	if err := sm.RunPostDeployStage(context.Background(), sm.Log); err != nil {
		t.Fatalf("RunPostDeployStage() = %v, want nil without reading the mirror", err)
	}

	sent := dropped.sentNotifications()
	if len(sent) != 1 || sent[0].level != notification.Success {
		t.Fatalf("RunPostDeployStage() sent %+v, want one success notification", sent)
	}

	if want := notification.GetRevision("main", revision); sent[0].metadata.Revision != want {
		t.Fatalf("notification revision = %q, want %q", sent[0].metadata.Revision, want)
	}

	sm.Notifier = &levelGatedSender{minLevel: notification.Success}
	if err := sm.RunPostDeployStage(context.Background(), sm.Log); err == nil {
		t.Fatal("RunPostDeployStage() = nil for a delivered notification, want the mirror read error")
	}
}

// TestPostDeployReportsShortCommitAndChangelog covers the delivered success
// notification: the short SHA and changelog come from one mirror read, and the
// short SHA is memoized for the rest of the deployment.
func TestPostDeployReportsShortCommitAndChangelog(t *testing.T) {
	t.Parallel()

	originPath, mirrorPath := setupOriginAndMirror(t)
	previous := mirrorHead(t, mirrorPath)

	writeFile(t, filepath.Join(originPath, "README.md"), "second\n")
	runGit(t, originPath, "add", ".")
	runGit(t, originPath, "commit", "-m", "second commit")
	runGitBare(t, mirrorPath, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")

	head := mirrorHead(t, mirrorPath)

	sender := &levelGatedSender{minLevel: notification.Success}
	sm := newNotifyingStageManager(mirrorPath, head, sender)
	sm.DeployState.DeployedCommit = previous

	if err := sm.RunPostDeployStage(context.Background(), sm.Log); err != nil {
		t.Fatalf("RunPostDeployStage() = %v", err)
	}

	sent := sender.sentNotifications()
	if len(sent) != 1 || sent[0].level != notification.Success {
		t.Fatalf("RunPostDeployStage() sent %+v, want one success notification", sent)
	}

	if want := notification.GetRevision("main", head[:7]); sent[0].metadata.Revision != want {
		t.Fatalf("notification revision = %q, want %q", sent[0].metadata.Revision, want)
	}

	commits := sent[0].metadata.Commits
	if len(commits) != 1 || commits[0].Hash != head || commits[0].Subject != "second commit" {
		t.Fatalf("notification changelog = %+v, want only the second commit", commits)
	}

	if short, ok := sm.cachedShortCommitSHA(head); !ok || short != head[:7] {
		t.Fatalf("cachedShortCommitSHA() = %q, %v, want %q memoized", short, ok, head[:7])
	}
}

// TestShortCommitSHAIsMemoized ensures a deployment shortens its commit once, and
// that a failed lookup is not cached as the answer.
func TestShortCommitSHAIsMemoized(t *testing.T) {
	t.Parallel()

	_, mirrorPath := setupOriginAndMirror(t)
	head := mirrorHead(t, mirrorPath)
	sm := newNotifyingStageManager(mirrorPath, head, &levelGatedSender{})

	unknown := strings.Repeat("0", 40)

	err := sm.withMirrorRead(func(repo *gogit.Repository) error {
		if got := sm.shortCommitSHA(repo, unknown); got != unknown {
			t.Errorf("shortCommitSHA(unknown) = %q, want the full SHA", got)
		}

		if got := sm.shortCommitSHA(repo, head); got != head[:7] {
			t.Errorf("shortCommitSHA(head) = %q, want %q", got, head[:7])
		}

		return nil
	})
	if err != nil {
		t.Fatalf("withMirrorRead() = %v", err)
	}

	if _, ok := sm.cachedShortCommitSHA(unknown); ok {
		t.Fatal("shortCommitSHA() memoized the fallback of a failed lookup")
	}

	// A nil repository is only safe to pass if the memoized value is returned.
	if got := sm.shortCommitSHA(nil, head); got != head[:7] {
		t.Fatalf("shortCommitSHA() second call = %q, want memoized %q", got, head[:7])
	}

	// The memo is keyed by the full SHA, so another revision is computed again.
	if got := sm.shortCommitSHA(nil, unknown); got != unknown {
		t.Fatalf("shortCommitSHA(nil, unknown) = %q, want the full SHA fallback", got)
	}
}
