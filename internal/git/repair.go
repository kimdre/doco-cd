package git

import (
	"errors"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/filesystem/dotgit"
)

// IsCorruptionError checks if an error indicates repository corruption rather than a transient failure.
func IsCorruptionError(err error) bool {
	if err == nil {
		return false
	}

	corruptionErrors := []error{
		plumbing.ErrReferenceNotFound,
		plumbing.ErrObjectNotFound,
		git.ErrInvalidReference,
		dotgit.ErrEmptyRefFile,
		dotgit.ErrPackedRefsBadFormat,
		dotgit.ErrPackedRefsDuplicatedRef,
		dotgit.ErrSymRefTargetNotFound,
	}

	if slices.ContainsFunc(corruptionErrors, func(target error) bool {
		return errors.Is(err, target)
	}) {
		return true
	}

	// patterns for common corruption-related error messages that may be wrapped by go-git
	patterns := []string{
		"reference not found",
		"object not found",
		"invalid reference",
	}

	// Check error message for corruption-related patterns
	msg := err.Error()

	return slices.ContainsFunc(patterns, func(pattern string) bool {
		return strings.Contains(msg, pattern)
	})
}
