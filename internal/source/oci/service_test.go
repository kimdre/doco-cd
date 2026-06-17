package oci

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateDocoLayoutV1_AcceptsVersionInConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".doco-cd.yaml"), []byte("version: doco.v1\nname: app\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := validateDocoLayoutV1(dir, ""); err != nil {
		t.Fatalf("validate layout: %v", err)
	}
}

func TestValidateDocoLayoutV1_AcceptsMissingVersion_DefaultsToV1(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".doco-cd.yaml"), []byte("name: app\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := validateDocoLayoutV1(dir, ""); err != nil {
		t.Fatalf("validate layout: %v", err)
	}
}

func TestValidateDocoLayoutV1_RejectsUnsupportedVersion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".doco-cd.yaml"), []byte("version: doco.v2\nname: app\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := validateDocoLayoutV1(dir, "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestValidateDocoLayoutV1_AcceptsCustomTargetConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".doco-cd.production.yaml"), []byte("version: doco.v1\nname: app\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := validateDocoLayoutV1(dir, "production"); err != nil {
		t.Fatalf("validate layout: %v", err)
	}
}

func TestValidateDocoLayoutV1_CustomTargetDoesNotFallBackToDefault(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Only the default config exists — custom target must NOT fall back to it.
	if err := os.WriteFile(filepath.Join(dir, ".doco-cd.yaml"), []byte("version: doco.v1\nname: app\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := validateDocoLayoutV1(dir, "production")
	if err == nil {
		t.Fatal("expected error when only default config exists and a custom target is set, got nil")
	}
}

func TestValidateDocoLayoutV1_CustomTargetMissingBothFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := validateDocoLayoutV1(dir, "staging")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestFindArtifactConfigFile_NoCustomTarget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".doco-cd.yml"), []byte("name: app\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	got, err := findArtifactConfigFile(dir, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if filepath.Base(got) != ".doco-cd.yml" {
		t.Fatalf("expected .doco-cd.yml, got %s", filepath.Base(got))
	}
}

func TestFindArtifactConfigFile_CustomTargetDoesNotFallBackToDefault(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Only the default config exists — should not be returned when a custom target is set.
	if err := os.WriteFile(filepath.Join(dir, ".doco-cd.yaml"), []byte("name: app\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := findArtifactConfigFile(dir, "production")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestFindArtifactConfigFile_CustomTargetReturnsTargetConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for _, name := range []string{".doco-cd.yaml", ".doco-cd.production.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("name: app\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	got, err := findArtifactConfigFile(dir, "production")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if filepath.Base(got) != ".doco-cd.production.yaml" {
		t.Fatalf("expected .doco-cd.production.yaml, got %s", filepath.Base(got))
	}
}
