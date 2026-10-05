package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func noArtifactBirthTime(string) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

func TestArtifactRepublicationRecoveryWithoutBirthTime(t *testing.T) {
	t.Parallel()

	for _, scenario := range []string{"restored identity", "missing directory", "empty replacement", "interrupted publication", "removed publication metadata"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()

			base := t.TempDir()
			write := func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "config"), []byte("intact"), 0o600)
			}
			artifact, err := publishDir(base, "rev", write)
			if err != nil {
				t.Fatal(err)
			}

			deployedAt := time.Now().UTC().Format(time.RFC3339Nano)
			if reason, err := artifactStaleReason(artifact.Path, deployedAt, noArtifactBirthTime); err != nil || reason != "" {
				t.Fatalf("first publication is stale: %q, %v", reason, err)
			}
			mounted, err := os.OpenRoot(artifact.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer mounted.Close()

			switch scenario {
			case "restored identity":
				// A backup copied the original .published record but restored a different inode.
				if err := os.WriteFile(artifact.Path+publishedSuffix, []byte("identity-before-restore"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing directory", "empty replacement", "removed publication metadata":
				if err := os.RemoveAll(artifact.Path); err != nil {
					t.Fatal(err)
				}
				if scenario == "empty replacement" {
					if err := os.Mkdir(artifact.Path, 0o755); err != nil {
						t.Fatal(err)
					}
					// Portable simulation of the old inode's identity record.
					if err := os.WriteFile(artifact.Path+publishedSuffix, []byte("old-inode"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "removed publication metadata" {
					for _, suffix := range []string{publishedSuffix, publicationTimeSuffix} {
						if err := os.Remove(artifact.Path + suffix); err != nil {
							t.Fatal(err)
						}
					}
				}
			case "interrupted publication":
				if err := recordArtifactPublication(artifact.Path); err != nil {
					t.Fatal(err)
				}
				if err := setAsideUnpublished(filepath.Dir(artifact.Path), artifact.Path); err != nil {
					t.Fatal(err)
				}
			}

			if _, found, err := lookupArtifact(base, "rev"); err != nil || found {
				t.Fatalf("restored artifact accepted: found=%v, err=%v", found, err)
			}
			if _, err := publishDir(base, "rev", write); err != nil {
				t.Fatal(err)
			}

			record, err := os.ReadFile(artifact.Path + publicationTimeSuffix)
			if err != nil {
				t.Fatal(err)
			}

			for range 2 { // Retry/restart checks have no process-local acknowledgement.
				reason, err := artifactStaleReason(artifact.Path, deployedAt, noArtifactBirthTime)
				if err != nil || reason != ArtifactReplaced {
					t.Fatalf("recovery without birth time = %q, %v", reason, err)
				}
				if _, err := publishDir(base, "rev", write); err != nil {
					t.Fatal(err)
				}
				current, err := os.ReadFile(artifact.Path + publicationTimeSuffix)
				if err != nil || string(current) != string(record) {
					t.Fatalf("cached publication changed recovery signal: %q, %v", current, err)
				}
			}

			recoveredAt := time.Now().UTC().Format(time.RFC3339Nano)
			if reason, err := artifactStaleReason(artifact.Path, recoveredAt, noArtifactBirthTime); err != nil || reason != "" {
				t.Fatalf("successful recreation repeats recovery: %q, %v", reason, err)
			}

			if scenario == "restored identity" {
				if data, err := mounted.ReadFile("config"); err != nil || string(data) != "intact" {
					t.Fatalf("mounted restored content = %q, %v", data, err)
				}

				entries, err := os.ReadDir(filepath.Dir(artifact.Path))
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if entry.IsDir() && entry.Name() != "rev" {
						path := filepath.Join(filepath.Dir(artifact.Path), entry.Name())
						old := time.Now().Add(-2 * orphanedTempMaxAge)
						if err := os.Chtimes(path, old, old); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := sweepOrphanedTemp(base); err != nil {
					t.Fatal(err)
				}
				if _, err := mounted.ReadFile("config"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("swept old mount still contains config: %v", err)
				}
				if reason, err := artifactStaleReason(artifact.Path, deployedAt, noArtifactBirthTime); err != nil || reason != ArtifactReplaced {
					t.Fatalf("sweeping erased recovery signal: %q, %v", reason, err)
				}
			}
		})
	}
}

func TestArtifactReplacementSameSecondWithoutBirthTime(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "artifact")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := recordArtifactMetadata(dir, publicationTimeSuffix, "2026-01-01T12:00:00.123456789Z"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		deployedAt, want string
	}{
		{"2026-01-01T12:00:00Z", ArtifactReplaced},
		{"2026-01-01T12:00:00.123456788Z", ArtifactReplaced},
		{"2026-01-01T12:00:00.123456790Z", ""},
	} {
		if reason, err := artifactStaleReason(dir, tc.deployedAt, noArtifactBirthTime); err != nil || reason != tc.want {
			t.Fatalf("deployment %s = %q, %v, want %q", tc.deployedAt, reason, err, tc.want)
		}
	}
}

func TestPublishDirReplacementRecordFailureLeavesOldDirectory(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	dir := filepath.Join(base, ArtifactsSubdir, "rev")
	if err := os.MkdirAll(dir+publicationTimeSuffix, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+publishedSuffix, []byte("restored-identity"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := publishDir(base, "rev", func(path string) error {
		return os.WriteFile(filepath.Join(path, "config"), []byte("new"), 0o600)
	})
	if err == nil {
		t.Fatal("publication succeeded without a durable replacement record")
	}
	if data, err := os.ReadFile(filepath.Join(dir, "config")); err != nil || string(data) != "old" {
		t.Fatalf("failed publication moved old mounted directory: %q, %v", data, err)
	}
}
