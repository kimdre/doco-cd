package git

import (
	"errors"
	"os"
	"regexp"

	"github.com/go-git/go-billy/v5/memfs"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/memory"
)

var NotImplementedError = errors.New("not implemented")

// CloneRepository clones a repository from a given URL and branch into the memory filesystem
func CloneRepository(url, ref string) (*git.Repository, error) {
	// Filesystem abstraction based on memory
	fs := memfs.New()
	// Git objects storer based on memory
	storer := memory.NewStorage()

	return git.Clone(storer, fs, &git.CloneOptions{
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
