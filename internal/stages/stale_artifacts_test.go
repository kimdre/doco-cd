package stages

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/source/store"
)

const (
	staleTestRevision = "1111111111111111111111111111111111111111"
	staleTestPinned   = "2222222222222222222222222222222222222222"
)

// newStaleArtifactTestStore returns the data mount source on the host, the data mount destination in this container
// and the store base of a repository in the data mount destination.
func newStaleArtifactTestStore(t *testing.T) (string, string, string) {
	t.Helper()

	destination := t.TempDir()
	storeBase := filepath.Join(destination, "git.example.com", "owner", "repo")

	return "/srv/doco-cd", destination, storeBase
}

func staleTestLabels(source, destination, storeBase, workingDir, deployedAt, pinned string) docker.Labels {
	rel, _ := filepath.Rel(destination, filepath.Join(storeBase, store.ArtifactsSubdir, store.ArtifactDirName(staleTestRevision), workingDir))

	labels := docker.Labels{
		docker.DocoCDLabels.Deployment.CommitSHA:  staleTestRevision,
		docker.DocoCDLabels.Deployment.WorkingDir: filepath.Join(source, rel),
		docker.DocoCDLabels.Deployment.Timestamp:  deployedAt,
	}

	if pinned != "" {
		labels[docker.DocoCDLabels.Deployment.PinnedRevisions] = pinned
	}

	return labels
}

func publishStaleTestArtifact(t *testing.T, storeBase, revision string) string {
	t.Helper()

	dir := filepath.Join(storeBase, store.ArtifactsSubdir, store.ArtifactDirName(store.Revision(revision)))
	if err := os.MkdirAll(dir, filesystem.PermDir); err != nil {
		t.Fatal(err)
	}

	return dir
}

func TestStaleArtifactServices(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	t.Run("artifact exists", func(t *testing.T) {
		t.Parallel()

		source, destination, storeBase := newStaleArtifactTestStore(t)
		publishStaleTestArtifact(t, storeBase, staleTestRevision)

		deployed := map[docker.Service]docker.ServiceStatus{
			"web": {Labels: staleTestLabels(source, destination, storeBase, "app", future, "")},
		}

		if got := staleArtifactServices(deployed, source, destination, log); len(got) != 0 {
			t.Fatalf("staleArtifactServices() = %+v, want none", got)
		}
	})

	t.Run("artifact missing", func(t *testing.T) {
		t.Parallel()

		source, destination, storeBase := newStaleArtifactTestStore(t)

		deployed := map[docker.Service]docker.ServiceStatus{
			"web": {Labels: staleTestLabels(source, destination, storeBase, "app", future, "")},
			"db":  {Labels: staleTestLabels(source, destination, storeBase, "", future, "")},
		}

		got := staleArtifactServices(deployed, source, destination, log)
		if len(got) != 2 || got[0].Service != "db" || got[1].Service != "web" {
			t.Fatalf("staleArtifactServices() = %+v, want db and web", got)
		}

		want := filepath.Join(storeBase, store.ArtifactsSubdir, store.ArtifactDirName(staleTestRevision))
		for _, stale := range got {
			if stale.Artifact != want || stale.Reason != artifactMissing {
				t.Fatalf("staleArtifactServices() = %+v, want artifact %s %s", stale, want, artifactMissing)
			}
		}

		change := staleArtifactChange(got)
		if change.Type != docker.ChangeTypeStaleArtifact || !slices.Equal(change.Services, []string{"db", "web"}) {
			t.Fatalf("staleArtifactChange() = %+v", change)
		}
	})

	t.Run("pinned artifact missing", func(t *testing.T) {
		t.Parallel()

		source, destination, storeBase := newStaleArtifactTestStore(t)
		publishStaleTestArtifact(t, storeBase, staleTestRevision)

		deployed := map[docker.Service]docker.ServiceStatus{
			"web": {Labels: staleTestLabels(source, destination, storeBase, "app", future, staleTestPinned)},
		}

		got := staleArtifactServices(deployed, source, destination, log)
		if len(got) != 1 || got[0].Artifact != filepath.Join(storeBase, store.ArtifactsSubdir, store.ArtifactDirName(staleTestPinned)) {
			t.Fatalf("staleArtifactServices() = %+v, want the pinned artifact", got)
		}
	})

	t.Run("pinned artifact exists", func(t *testing.T) {
		t.Parallel()

		source, destination, storeBase := newStaleArtifactTestStore(t)
		publishStaleTestArtifact(t, storeBase, staleTestPinned)

		// Only the pinned artifact is mounted, so the missing artifact of the deployed revision does not matter.
		deployed := map[docker.Service]docker.ServiceStatus{
			"web": {Labels: staleTestLabels(source, destination, storeBase, "app", future, staleTestPinned)},
		}

		if got := staleArtifactServices(deployed, source, destination, log); len(got) != 0 {
			t.Fatalf("staleArtifactServices() = %+v, want none", got)
		}
	})

	t.Run("not deployed from an artifact", func(t *testing.T) {
		t.Parallel()

		source, destination, _ := newStaleArtifactTestStore(t)

		deployed := map[docker.Service]docker.ServiceStatus{
			"legacy": {Labels: docker.Labels{
				docker.DocoCDLabels.Deployment.CommitSHA:  staleTestRevision,
				docker.DocoCDLabels.Deployment.WorkingDir: filepath.Join(source, "git.example.com", "owner", "repo", "app"),
			}},
			"outside": {Labels: docker.Labels{
				docker.DocoCDLabels.Deployment.CommitSHA:  staleTestRevision,
				docker.DocoCDLabels.Deployment.WorkingDir: filepath.Join("/elsewhere", store.ArtifactsSubdir, staleTestRevision),
			}},
			"unlabeled": {},
		}

		if got := staleArtifactServices(deployed, source, destination, log); len(got) != 0 {
			t.Fatalf("staleArtifactServices() = %+v, want none", got)
		}
	})

	t.Run("artifact published again after the deployment", func(t *testing.T) {
		t.Parallel()

		source, destination, storeBase := newStaleArtifactTestStore(t)
		dir := publishStaleTestArtifact(t, storeBase, staleTestRevision)

		if _, ok, err := filesystem.BirthTime(dir); err != nil || !ok {
			t.Skipf("file system does not record creation times: %v", err)
		}

		past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		justNow := time.Now().UTC().Format(time.RFC3339)

		deployed := map[docker.Service]docker.ServiceStatus{
			"old": {Labels: staleTestLabels(source, destination, storeBase, "app", past, "")},
			// The deployment time is truncated to seconds, so an artifact published in the same second is not stale.
			"new":       {Labels: staleTestLabels(source, destination, storeBase, "app", justNow, "")},
			"no-record": {Labels: staleTestLabels(source, destination, storeBase, "app", "", "")},
		}

		got := staleArtifactServices(deployed, source, destination, log)
		if len(got) != 1 || got[0].Service != "old" || got[0].Reason != artifactReplaced || got[0].Artifact != dir {
			t.Fatalf("staleArtifactServices() = %+v, want old %s", got, artifactReplaced)
		}
	})
}

func TestDataMountPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, hostPath, want string
		ok                   bool
	}{
		{name: "below the data mount", hostPath: "/srv/doco-cd/repo/artifacts/abc", want: "/data/repo/artifacts/abc", ok: true},
		{name: "data mount itself", hostPath: "/srv/doco-cd", want: "/data", ok: true},
		{name: "outside the data mount", hostPath: "/srv/other/repo", ok: false},
		{name: "sibling with common prefix", hostPath: "/srv/doco-cd-old/repo", ok: false},
		{name: "relative", hostPath: "repo", ok: false},
		{name: "empty", hostPath: " ", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := dataMountPath(tt.hostPath, "/srv/doco-cd", "/data")
			if got != tt.want || ok != tt.ok {
				t.Fatalf("dataMountPath(%q) = %q, %v, want %q, %v", tt.hostPath, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestArtifactStoreBase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, path, want string
		ok               bool
	}{
		{name: "artifact root", path: "/data/repo/artifacts/" + staleTestRevision, want: "/data/repo", ok: true},
		{name: "working directory in artifact", path: "/data/repo/artifacts/" + staleTestRevision + "/app/sub", want: "/data/repo", ok: true},
		{name: "other revision", path: "/data/repo/artifacts/" + staleTestPinned + "/app", ok: false},
		{name: "not an artifact", path: "/data/repo/" + staleTestRevision + "/app", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := artifactStoreBase(tt.path, staleTestRevision)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("artifactStoreBase(%q) = %q, %v, want %q, %v", tt.path, got, ok, tt.want, tt.ok)
			}
		})
	}
}
