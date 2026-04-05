package bitwardenvault

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kimdre/doco-cd/internal/filesystem"
)

type asset struct {
	Name        string `json:"name"`
	DownloadUrl string `json:"browser_download_url"`
	Size        int64  `json:"size"`
}

type release struct {
	Name   string  `json:"tag_name"`
	Assets []asset `json:"assets"`
}

// Queries the Bitwarden GitHub API for the latest CLI release.
func getLatestRelease() (*release, error) {
	const (
		releasesUrl = "https://api.github.com/repos/bitwarden/clients/releases"
		tagPrefix   = "cli-"
	)

	// Get releases from GitHub API
	resp, err := http.Get(releasesUrl)
	if err != nil {
		return nil, fmt.Errorf("failed to query Bitwarden GitHub releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub releases API returned status %d", resp.StatusCode)
	}

	var releases []release

	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("failed to decode GitHub releases JSON: %w", err)
	}

	// Find latest tag with prefix
	var latestRelease *release

	for _, r := range releases {
		if strings.HasPrefix(r.Name, tagPrefix) {
			latestRelease = &r
			break
		}
	}

	if latestRelease == nil {
		return nil, errors.New("no Bitwarden CLI release found")
	}

	return latestRelease, nil
}

// download the CLI for the current OS/arch from the given release and save it to binDir with the name binName.
func downloadCLI(r *release, binDir, binName string) error {
	cliPath := filepath.Join(binDir, binName)

	// Determine asset name pattern
	var assetPattern string

	switch runtime.GOOS {
	case "linux":
		switch runtime.GOARCH {
		case "amd64":
			assetPattern = "bw-linux-"
		case "arm64":
			assetPattern = "bw-linux-arm64-"
		default:
			return fmt.Errorf("%w: %s", ErrUnsupportedArch, runtime.GOARCH)
		}
	case "darwin":
		switch runtime.GOARCH {
		case "amd64":
			assetPattern = "bw-macos-"
		case "arm64":
			assetPattern = "bw-macos-arm64-"
		default:
			return fmt.Errorf("%w: %s", ErrUnsupportedArch, runtime.GOARCH)
		}
	default:
		return fmt.Errorf("%w: %s", ErrUnsupportedOS, runtime.GOOS)
	}

	var (
		assetURL, assetName string
		assetSize           int64
	)

	for _, a := range r.Assets {
		if strings.HasPrefix(a.Name, assetPattern) && strings.HasSuffix(a.Name, ".zip") {
			assetURL = a.DownloadUrl
			assetName = a.Name
			assetSize = a.Size * 1024

			break
		}
	}

	if assetURL == "" {
		return fmt.Errorf("could not find Bitwarden CLI asset for %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	zipPath := filepath.Join(binDir, assetName)

	out, err := os.Create(zipPath)
	if err != nil {
		return fmt.Errorf("failed to create zip file: %w", err)
	}

	defer os.Remove(zipPath)

	defer func() {
		cerr := out.Close()
		if cerr != nil && err == nil {
			err = fmt.Errorf("failed to close zip file: %w", cerr)
		}
	}()

	resp, err := http.Get(assetURL) // #nosec G107 -- URL is from trusted GitHub API response, not user-controlled
	if err != nil {
		return fmt.Errorf("failed to download bw CLI: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download bw CLI: status %d", resp.StatusCode)
	}

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to save bw CLI zip: %w", err)
	}
	// Unzip
	zipReader, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("failed to open bw CLI zip: %w", err)
	}

	defer func() {
		cerr := zipReader.Close()
		if cerr != nil && err == nil {
			err = fmt.Errorf("failed to close zip reader: %w", cerr)
		}
	}()

	// Iterate through zip files to find the CLI binary
	for _, f := range zipReader.File {
		// Only match the binary at the root (not in a subfolder)
		if filepath.Base(f.Name) == binName {
			// Create the destination file
			binFile, ferr := os.OpenFile(cliPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, filesystem.PermExecutable)
			if ferr != nil {
				return fmt.Errorf("failed to create bw CLI binary: %w", ferr)
			}

			// Open the file inside the archive
			rc, ferr := f.Open()
			if ferr != nil {
				_ = binFile.Close()
				return fmt.Errorf("failed to open bw CLI file in zip: %w", ferr)
			}

			_, ferr = io.Copy(binFile, io.LimitReader(rc, assetSize))
			rcerr := rc.Close()
			bfcerr := binFile.Close()

			if ferr != nil {
				return fmt.Errorf("failed to extract bw CLI: %w", ferr)
			}

			if rcerr != nil {
				return fmt.Errorf("failed to close zip file reader: %w", rcerr)
			}

			if bfcerr != nil {
				return fmt.Errorf("failed to close bw CLI binary: %w", bfcerr)
			}

			return nil
		}
	}

	return fmt.Errorf("%w: %s", os.ErrNotExist, binName)
}
