package git

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// watchDebounce is how long to wait after a filesystem event before forwarding
// the signal, coalescing rapid sequences of events (e.g. a git commit touching
// multiple ref files).
const watchDebounce = 300 * time.Millisecond

// WatchLocalGitRef watches a local git repository for changes to branch refs or
// packed-refs, signaling on the returned channel whenever new commits may have
// landed.
//
// The channel is buffered (capacity 1) so a missed tick is never blocking; at
// most one pending notification is queued at a time. The watcher stops and
// closes the channel when ctx is canceled.
//
// repoURL must be a file:// URL (e.g. "file:///path/to/repo").
func WatchLocalGitRef(ctx context.Context, repoURL string, log *slog.Logger) (<-chan struct{}, error) {
	repoPath, err := localPathFromURL(repoURL)
	if err != nil {
		return nil, err
	}

	gitDir, err := resolveLocalGitDir(repoPath)
	if err != nil {
		return nil, fmt.Errorf("cannot watch %q: %w", repoPath, err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("failed to create filesystem watcher: %w", err)
	}

	// Watch the git directory root so we catch packed-refs updates.
	if err := watcher.Add(gitDir); err != nil {
		_ = watcher.Close()
		return nil, fmt.Errorf("failed to watch git directory %q: %w", gitDir, err)
	}

	// Best-effort: also watch refs/heads/ for loose-ref updates.
	refsHeadsDir := filepath.Join(gitDir, "refs", "heads")
	if _, statErr := os.Stat(refsHeadsDir); statErr == nil {
		_ = watcher.Add(refsHeadsDir)
	}

	packedRefs := filepath.Join(gitDir, "packed-refs")
	ch := make(chan struct{}, 1)

	go func() {
		defer func() { _ = watcher.Close() }()

		var debounce *time.Timer

		var debounceMu sync.Mutex

		stopped := false

		defer func() {
			if debounce != nil {
				debounce.Stop()
			}

			debounceMu.Lock()
			stopped = true

			for {
				select {
				case <-ch:
				default:
					close(ch)
					debounceMu.Unlock()

					return
				}
			}
		}()

		send := func() {
			debounceMu.Lock()
			defer debounceMu.Unlock()

			if stopped {
				return
			}

			select {
			case ch <- struct{}{}:
			default: // already pending
			}
		}

		for {
			select {
			case <-ctx.Done():
				if debounce != nil {
					debounce.Stop()
				}

				return

			case event, ok := <-watcher.Events:
				if !ok {
					return
				}

				cleanName := filepath.Clean(event.Name)

				// Ignore lock files created during atomic ref updates.
				if strings.HasSuffix(cleanName, ".lock") {
					continue
				}

				isPackedRefs := cleanName == packedRefs
				isLooseRef := strings.HasPrefix(cleanName, refsHeadsDir+string(filepath.Separator))

				if !isPackedRefs && !isLooseRef {
					continue
				}

				if debounce != nil {
					debounce.Stop()
				}

				debounce = time.AfterFunc(watchDebounce, func() {
					send()
				})

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}

				if log != nil {
					log.Warn("filesystem watcher error", slog.Any("error", err))
				}
			}
		}
	}()

	return ch, nil
}

// localPathFromURL extracts the filesystem path from a file:// URL.
func localPathFromURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}

	if u.Scheme != "file" {
		return "", fmt.Errorf("expected file:// URL, got %q", rawURL)
	}

	return filepath.FromSlash(u.Path), nil
}

// resolveLocalGitDir returns the path to the git directory of the repository at
// repoPath. It handles bare repos, standard repos (.git directory), and
// worktree/.git-file layouts.
func resolveLocalGitDir(repoPath string) (string, error) {
	// Bare repo: config file lives directly in the root directory.
	if _, err := os.Stat(filepath.Join(repoPath, "config")); err == nil {
		return repoPath, nil
	}

	gitPath := filepath.Join(repoPath, ".git")

	info, err := os.Stat(gitPath)
	if err != nil {
		return "", fmt.Errorf("not a git repository: %q", repoPath)
	}

	if info.IsDir() {
		return gitPath, nil
	}

	// .git is a file (linked worktree or submodule); read the gitdir pointer.
	raw, err := os.ReadFile(gitPath)
	if err != nil {
		return "", fmt.Errorf("cannot read .git file at %q: %w", gitPath, err)
	}

	content := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(content, "gitdir:") {
		return "", fmt.Errorf("malformed .git file at %q", gitPath)
	}

	target := strings.TrimSpace(strings.TrimPrefix(content, "gitdir:"))
	if !filepath.IsAbs(target) {
		target = filepath.Join(repoPath, target)
	}

	target = filepath.Clean(target)

	// Linked worktrees share the main repo's object store via a "commondir" file.
	if data, err := os.ReadFile(filepath.Join(target, "commondir")); err == nil {
		commonDir := strings.TrimSpace(string(data))
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(target, commonDir)
		}

		return filepath.Clean(commonDir), nil
	}

	return target, nil
}
