package bitwardenvault

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetLatestRelease(t *testing.T) {
	t.Parallel()

	r, err := getLatestRelease()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if r == nil {
		t.Fatal("expected a non-nil release")
		return
	}

	t.Logf("Latest Bitwarden CLI release: %s", r.Name)

	for _, a := range r.Assets {
		if strings.HasPrefix(a.Name, "bw-") && strings.HasSuffix(a.Name, ".zip") {
			t.Logf("Asset: %s, Download URL: %s, Size: %d", a.Name, a.DownloadUrl, a.Size)
		}
	}
}

func TestDownloadCLI(t *testing.T) {
	releaseJSON := `{
  "tag_name": "cli-v2026.2.0",
  "assets": [
    {
      "name": "bitwarden-cli-2026.2.0-npm-build.zip",
      "browser_download_url": "https://github.com/bitwarden/clients/releases/download/cli-v2026.2.0/bitwarden-cli-2026.2.0-npm-build.zip",
      "size": 3745565
    },
    {
      "name": "bitwarden-cli.2026.2.0.nupkg",
      "browser_download_url": "https://github.com/bitwarden/clients/releases/download/cli-v2026.2.0/bitwarden-cli.2026.2.0.nupkg",
      "size": 38848045
    },
    {
      "name": "bw-linux-2026.2.0.zip",
      "browser_download_url": "https://github.com/bitwarden/clients/releases/download/cli-v2026.2.0/bw-linux-2026.2.0.zip",
      "size": 44210032
    },
    {
      "name": "bw-linux-arm64-2026.2.0.zip",
      "browser_download_url": "https://github.com/bitwarden/clients/releases/download/cli-v2026.2.0/bw-linux-arm64-2026.2.0.zip",
      "size": 42724133
    },
    {
      "name": "bw-macos-2026.2.0.zip",
      "browser_download_url": "https://github.com/bitwarden/clients/releases/download/cli-v2026.2.0/bw-macos-2026.2.0.zip",
      "size": 42497604
    },
    {
      "name": "bw-macos-arm64-2026.2.0.zip",
      "browser_download_url": "https://github.com/bitwarden/clients/releases/download/cli-v2026.2.0/bw-macos-arm64-2026.2.0.zip",
      "size": 40224813
    },
    {
      "name": "bw-oss-linux-2026.2.0.zip",
      "browser_download_url": "https://github.com/bitwarden/clients/releases/download/cli-v2026.2.0/bw-oss-linux-2026.2.0.zip",
      "size": 44182432
    },
    {
      "name": "bw-oss-linux-arm64-2026.2.0.zip",
      "browser_download_url": "https://github.com/bitwarden/clients/releases/download/cli-v2026.2.0/bw-oss-linux-arm64-2026.2.0.zip",
      "size": 42683856
    },
    {
      "name": "bw-oss-macos-2026.2.0.zip",
      "browser_download_url": "https://github.com/bitwarden/clients/releases/download/cli-v2026.2.0/bw-oss-macos-2026.2.0.zip",
      "size": 42463642
    },
    {
      "name": "bw-oss-macos-arm64-2026.2.0.zip",
      "browser_download_url": "https://github.com/bitwarden/clients/releases/download/cli-v2026.2.0/bw-oss-macos-arm64-2026.2.0.zip",
      "size": 40199895
    },
    {
      "name": "bw-oss-windows-2026.2.0.zip",
      "browser_download_url": "https://github.com/bitwarden/clients/releases/download/cli-v2026.2.0/bw-oss-windows-2026.2.0.zip",
      "size": 37509796
    },
    {
      "name": "bw-windows-2026.2.0.zip",
      "browser_download_url": "https://github.com/bitwarden/clients/releases/download/cli-v2026.2.0/bw-windows-2026.2.0.zip",
      "size": 37537283
    },
    {
      "name": "bw_2026.2.0_amd64.snap",
      "browser_download_url": "https://github.com/bitwarden/clients/releases/download/cli-v2026.2.0/bw_2026.2.0_amd64.snap",
      "size": 37048320
    }
  ]
}`

	var r *release

	err := json.Unmarshal([]byte(releaseJSON), &r)
	if err != nil {
		t.Fatalf("failed to unmarshal release JSON: %v", err)
	}

	if r == nil {
		t.Fatal("expected a non-nil release")
		return
	}

	binDir := t.TempDir()

	const binName = "bw"

	err = downloadCLI(r, binDir, binName)
	if err != nil {
		t.Fatalf("unexpected error in CLI download: %v", err)
	}

	cliPath := filepath.Join(binDir, binName)
	if _, err = os.Stat(cliPath); err != nil {
		t.Fatalf("expected CLI binary to be downloaded at %s, but got error: %v", cliPath, err)
	}

	// Run download executable
	err = exec.Command(cliPath, "--version").Run()
	if err != nil {
		t.Fatalf("failed to execute downloaded CLI: %v", err)
	}
}
