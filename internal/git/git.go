package git

import (
	"fmt"
	"os"
	"regexp"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// CloneRepository clones a repository from a given URL and branch into the memory filesystem
func CloneRepository(name, url, ref string) (*git.Repository, error) {
	dir := fmt.Sprintf("%s/%s", os.TempDir(), name)
	err := os.MkdirAll(dir, os.ModePerm)
	if err != nil {
		return nil, err
	}

	return git.PlainClone(dir, false, &git.CloneOptions{
		URL:           url,
		SingleBranch:  true,
		ReferenceName: plumbing.ReferenceName(ref),
		Tags:          git.NoTags,
		Depth:         1,
	})
}

// GetAuthUrl returns a clone URL with an access token for private repositories
func GetAuthUrl(url, token string) string {
	// Retrieve the protocol from the clone URL (e.g. https://, http://, git://
	protocol := regexp.MustCompile("^(https?|git)://").FindString(url)
	return protocol + token + "@" + url[len(protocol):]
}
