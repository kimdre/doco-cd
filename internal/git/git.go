package git

import (
	"errors"
	"fmt"
	"os"

	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/kimdre/docker-compose-webhook/internal/logger"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/memory"
)

var (
	NotImplementedError = errors.New("not implemented")
	log                 = logger.GetLogger()
)

// CloneRepository clones a repository from a given URL and branch into the memory filesystem
func CloneRepository(url, branch string, auth transport.AuthMethod) (*git.Repository, error) {
	log.Info(fmt.Sprintf("Cloning repository %s (%s)", url, branch))

	// Filesystem abstraction based on memory
	fs := memfs.New()
	// Git objects storer based on memory
	storer := memory.NewStorage()

	repo, err := git.Clone(storer, fs, &git.CloneOptions{
		Auth:          auth,
		URL:           url,
		SingleBranch:  true,
		ReferenceName: plumbing.ReferenceName(branch),
		Tags:          git.NoTags,
		Depth:         1,
	})

	return repo, err
}

func DirectoryExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}
