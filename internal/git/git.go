package git

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// CloneRepository clones a repository from a given URL and reference to a temporary directory
func CloneRepository(name, url, ref string, skipTLSVerify bool) (*git.Repository, error) {
	path := filepath.Join(os.TempDir(), name)

	err := os.MkdirAll(path, os.ModePerm)
	if err != nil {
		return nil, err
	}

	fmt.Printf("cloning on path: %s", path)

	return git.PlainClone(path, false, &git.CloneOptions{
		URL:             url,
		SingleBranch:    true,
		ReferenceName:   plumbing.ReferenceName(ref),
		Tags:            git.NoTags,
		Depth:           1,
		InsecureSkipTLS: skipTLSVerify,
	})
}

// GetAuthUrl returns a clone URL with an access token for private repositories
func GetAuthUrl(url, authType, token string) string {
	// Retrieve the protocol from the clone URL (e.g. https://, http://, git://
	protocol := regexp.MustCompile("^(https?|git)://").FindString(url)
	return protocol + authType + ":" + token + "@" + url[len(protocol):]
}
