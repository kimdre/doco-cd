package oci

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateDocoLayoutV1_AcceptsLayoutInConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".doco-cd.yaml"), []byte("layout: doco.v1\nname: app\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := validateDocoLayoutV1(dir); err != nil {
		t.Fatalf("validate layout: %v", err)
	}
}

func TestValidateDocoLayoutV1_RejectsMissingLayout(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".doco-cd.yaml"), []byte("name: app\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := validateDocoLayoutV1(dir)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
