package git

import (
	"errors"
	"net"
	"net/url"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/kimdre/doco-cd/internal/common/errdecode"
)

var (
	ErrMissingAuthToken           = errors.New("missing access token")
	ErrCheckoutFailed             = errors.New("failed to checkout repository")
	ErrFetchFailed                = errors.New("failed to fetch repository")
	ErrRepositoryNotExists        = git.ErrRepositoryNotExists
	ErrRepositoryAlreadyExists    = git.ErrRepositoryAlreadyExists
	ErrInvalidReference           = git.ErrInvalidReference
	ErrSSHKeyRequired             = errors.New("ssh URL requires SSH_PRIVATE_KEY to be set")
	ErrPossibleAuthMethodMismatch = errors.New("there might be a mismatch between the authentication method and the repository or submodule remote URL")
	ErrRemoteURLMismatch          = errors.New("remote URL does not match expected URL")
	ErrGetHeadFailed              = errors.New("failed to get HEAD reference")
)

// IsRefUnreachableError returns true when the error indicates the requested ref
// could not be found, which in a shallow clone likely means the ref is outside
// the fetched depth.
func IsRefUnreachableError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, plumbing.ErrObjectNotFound) ||
		errors.Is(err, ErrInvalidReference) ||
		errors.Is(err, plumbing.ErrReferenceNotFound) {
		return true
	}

	// go-git may wrap the object-not-found message in other errors
	msg := err.Error()

	return strings.Contains(msg, "object not found") ||
		strings.Contains(msg, "reference not found")
}

// isNonRecoverableError returns true for auth, network, or proxy errors that
// should NOT trigger a deepen attempt.
func isNonRecoverableError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, transport.ErrAuthenticationRequired) ||
		errors.Is(err, transport.ErrAuthorizationFailed) ||
		errors.Is(err, transport.ErrInvalidAuthMethod) {
		return true
	}

	_, isURLErr := errors.AsType[*url.Error](err)
	netErr, isNetErr := errors.AsType[net.Error](err)

	return isURLErr || (isNetErr && netErr.Timeout())
}

// FormatGitErrorMessage formats a git operation error message by attempting to decode
// any localized error response bodies embedded in it (e.g. from Azure DevOps Server).
// It returns the formatted error message suitable for logging.
func FormatGitErrorMessage(err error) string {
	if err == nil {
		return ""
	}

	return errdecode.DecodeEmbeddedJSON(err.Error())
}
