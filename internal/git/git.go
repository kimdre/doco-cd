package git

import (
	"errors"
	"os"
	"regexp"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

var NotImplementedError = errors.New("not implemented")

// CloneRepository clones a repository from a given URL and branch into the memory filesystem
func CloneRepository(name, url, ref string) (*git.Repository, error) {
	// Create a temporary directory with a unique name
	tempDir, err := os.MkdirTemp(os.TempDir(), "deploy-*")
	if err != nil {
		return nil, err
	}

	return git.PlainClone(tempDir, false, &git.CloneOptions{
		URL:           url,
		SingleBranch:  true,
		ReferenceName: plumbing.ReferenceName(ref),
		Tags:          git.NoTags,
		Depth:         1,
	})
}

func DirectoryExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}

// GetAuthUrl returns a clone URL with an access token for private repositories
func GetAuthUrl(url, token string) string {
	// Retrieve the protocol from the clone URL (e.g. https://, http://, git://
	protocol := regexp.MustCompile("^(https?|git)://").FindString(url)
	return protocol + token + "@" + url[len(protocol):]
}
