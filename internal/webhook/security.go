package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

var (
	ErrHMACVerificationFailed        = errors.New("HMAC verification failed")
	ErrGitlabTokenVerificationFailed = errors.New("gitlab token verification failed")
	ErrMissingSecurityHeader         = errors.New("missing signature or token header")
)

const (
	GithubSignatureHeader = "X-Hub-Signature-256"
	GiteaSignatureHeader  = "X-Gitea-Signature"
	GitlabTokenHeader     = "X-Gitlab-Token"
)

func verifySignature(payload []byte, signature, secretKey string) error {
	mac := hmac.New(sha256.New, []byte(secretKey))
	mac.Write(payload)

	expectedMAC := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(signature), []byte(expectedMAC)) {
		return ErrHMACVerificationFailed
	} else {
		return nil
	}
}

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
