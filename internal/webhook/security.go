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
	ErrIncorrectSecretKey            = errors.New("incorrect secret key")
	ErrHMACVerificationFailed        = errors.New("HMAC verification failed")
	ErrGitlabTokenVerificationFailed = errors.New("gitlab token verification failed")
	ErrMissingSecurityHeader         = errors.New("missing signature or token header")
)

type ScmProvider int // ScmProvider represents the supported source code management provider.

const (
	Unknown ScmProvider = iota
	Github
	Gitlab
	Gitea
	Gogs
)

// ScmProviderSecurityHeaders maps ScmProvider to their respective security header names.
var ScmProviderSecurityHeaders = map[ScmProvider]string{
	Github: "X-Hub-Signature-256",
	Gitlab: "X-Gitlab-Token", // #nosec G101
	Gitea:  "X-Gitea-Signature",
	Gogs:   "X-Gogs-Signature",
}

func GenerateHMAC(payload []byte, secretKey string) string {
	mac := hmac.New(sha256.New, []byte(secretKey))
	mac.Write(payload)

	return hex.EncodeToString(mac.Sum(nil))
}

func verifySignature(payload []byte, signature, secretKey string) error {
	expectedMAC := GenerateHMAC(payload, secretKey)
	if !hmac.Equal([]byte(signature), []byte(expectedMAC)) {
		return ErrHMACVerificationFailed
	}

	return nil
}

// VerifyProviderSecret checks and verifies the security header and returns the provider if verification is successful.
func verifyProviderSecret(r *http.Request, payload []byte, secretKey string) (ScmProvider, error) {
	switch {
	case r.Header.Get(ScmProviderSecurityHeaders[Github]) != "":
		signature := strings.TrimPrefix(r.Header.Get(ScmProviderSecurityHeaders[Github]), "sha256=")

		return Github, verifySignature(payload, signature, secretKey)

	case r.Header.Get(ScmProviderSecurityHeaders[Gitea]) != "":
		signature := r.Header.Get(ScmProviderSecurityHeaders[Gitea])

		return Gitea, verifySignature(payload, signature, secretKey)

	case r.Header.Get(ScmProviderSecurityHeaders[Gitlab]) != "":
		if secretKey != r.Header.Get(ScmProviderSecurityHeaders[Gitlab]) {
			return Gitlab, ErrGitlabTokenVerificationFailed
		}

		return Gitlab, nil

	case r.Header.Get(ScmProviderSecurityHeaders[Gogs]) != "":
		signature := r.Header.Get(ScmProviderSecurityHeaders[Gogs])

		return Gogs, verifySignature(payload, signature, secretKey)

	default:
		return Unknown, ErrMissingSecurityHeader
	}
}
