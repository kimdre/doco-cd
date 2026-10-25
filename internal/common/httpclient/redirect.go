// Package httpclient contains helpers for HTTP clients that send credentials.
package httpclient

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ErrCrossOriginRedirect is returned when a request with credentials is redirected to another origin.
var ErrCrossOriginRedirect = errors.New("redirect to a different origin was blocked")

// maxRedirects matches the limit of the default net/http client.
const maxRedirects = 10

// SameOriginRedirects is an http.Client CheckRedirect function for requests that carry credentials.
// It follows a redirect only when scheme, host and port stay the same as in the first request.
// net/http still forwards the Authorization header to subdomains, other ports and from https to http.
// This function blocks those redirects before a request reaches the new target.
func SameOriginRedirects(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}

	if len(via) == 0 {
		return nil
	}

	if !SameOrigin(via[0].URL, req.URL) {
		return fmt.Errorf("%w: %s to %s", ErrCrossOriginRedirect, Origin(via[0].URL), Origin(req.URL))
	}

	return nil
}

// SameOrigin reports whether two URLs have the same scheme, host and effective port.
func SameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}

	return Origin(a) == Origin(b)
}

// Origin returns scheme://host:port for a URL, with the default port filled in.
// It contains no user info, path or query, so it is safe to log.
func Origin(u *url.URL) string {
	if u == nil {
		return ""
	}

	scheme := strings.ToLower(u.Scheme)
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")

	port := u.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}

	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}

	return scheme + "://" + host + ":" + port
}
