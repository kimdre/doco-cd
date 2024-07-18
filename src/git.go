package main

import (
	"fmt"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"os"
)

func CloneRepository(url, path, branch string, isPrivate bool) error {
	if isPrivate {
		// Implement authentication here
	}

	fmt.Printf("Cloning repository %s (%s) into %s\n", url, branch, path)

	_, err := git.PlainClone(path, false, &git.CloneOptions{
		URL:           url,
		Progress:      os.Stdout,
		SingleBranch:  true,
		ReferenceName: plumbing.ReferenceName(branch),
		Tags:          git.NoTags,
		Depth:         1,
		// add authentication here
	})

	return err
}

func DirectoryExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}
