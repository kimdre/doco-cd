package git

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func httpStatusErr(code int) error {
	return plumbing.NewUnexpectedError(&githttp.Err{Response: &http.Response{StatusCode: code}})
}

func TestIsTransientError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"url error", &url.Error{Op: "Get", URL: "https://example.com", Err: errors.New("TLS handshake timeout")}, true},
		{"net timeout", timeoutError{}, true},
		{"net timeout wrapped", fmt.Errorf("fetch: %w", &net.OpError{Op: "dial", Err: timeoutError{}}), true},
		{"http 503", httpStatusErr(http.StatusServiceUnavailable), true},
		{"http 500", httpStatusErr(http.StatusInternalServerError), true},
		{"http 502 wrapped", fmt.Errorf("%w: %w", ErrFetchFailed, httpStatusErr(http.StatusBadGateway)), true},
		{"http 429", httpStatusErr(http.StatusTooManyRequests), true},
		{"http 400", httpStatusErr(http.StatusBadRequest), false},
		{"auth required", transport.ErrAuthenticationRequired, false},
		{"repo not found", transport.ErrRepositoryNotFound, false},
		{"unexpected non-http", plumbing.NewUnexpectedError(errors.New("boom")), false},
		{"plain error", errors.New("boom"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := isTransientError(tt.err); got != tt.want {
				t.Errorf("isTransientError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
