package git

import (
	"testing"
	
	"github.com/go-git/go-git/v5/plumbing/transport"
	
	"github.com/kimdre/doco-cd/internal/config"
)

func TestFinalCloneHTTP(t *testing.T) {
	cfg, err := config.GetAppConfig()
	if err != nil {
		t.Fatalf("Failed to get config: %v", err)
	}
	
	if cfg.GitAccessToken == "" {
		t.Skip("No token provided")
	}
	
	t.Log("Using HTTP token auth")
	
	// Call HttpTokenAuth
	auth := HttpTokenAuth(cfg.GitAccessToken)
	t.Logf("Auth type: %T", auth)
	
	// Call CloneRepository
	tmpDir := t.TempDir()
	
	repo, err := CloneRepository(
		tmpDir,
		"https://github.com/kimdre/doco-cd.git",
		"refs/heads/main",
		false,
		transport.ProxyOptions{},
		auth,
		false,
	)
	
	if err != nil {
		t.Fatalf("Clone failed: %v", err)
	}
	
	if repo == nil {
		t.Fatal("Repo is nil")
	}
	
	t.Log("SUCCESS!")
}
