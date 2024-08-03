package webhook

import (
	"errors"
	"io"
	"net/http"
	"strings"
)

var (
	ErrInvalidHTTPMethod     = errors.New("invalid http method")
	ErrMissingSecurityHeader = errors.New("missing signature or token header")
	ErrParsingPayload        = errors.New("failed to parse payload")
)

// VerifyProviderSecret checks and verifies the security header and returns the provider if verification is successful
func verifyProviderSecret(r *http.Request, payload []byte, secretKey string) (string, error) {
	switch {
	case r.Header.Get(GithubSignatureHeader) != "":
		signature := strings.TrimPrefix(r.Header.Get(GithubSignatureHeader), "sha256=")
		return "github", verifySignature(payload, signature, secretKey)

	case r.Header.Get(GiteaSignatureHeader) != "":
		signature := r.Header.Get(GiteaSignatureHeader)
		return "gitea", verifySignature(payload, signature, secretKey)

	case r.Header.Get(GitlabTokenHeader) != "":
		if secretKey != r.Header.Get(GitlabTokenHeader) {
			return "", ErrGitlabTokenVerificationFailed
		}

		return "gitlab", nil

	default:
		return "", ErrMissingSecurityHeader
	}
}

// Parse parses the payload and returns the parsed payload data
func Parse(r *http.Request, secretKey string) (ParsedPayload, error) {
	defer func() {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}()

	if r.Method != http.MethodPost {
		return ParsedPayload{}, ErrInvalidHTTPMethod
	}

	payload, err := io.ReadAll(r.Body)
	if err != nil || len(payload) == 0 {
		return ParsedPayload{}, err
	}

	provider, err := verifyProviderSecret(r, payload, secretKey)
	if err != nil {
		return ParsedPayload{}, err
	}

	return parsePayload(payload, provider)
}
