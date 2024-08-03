package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

var (
	ErrHMACVerificationFailed        = errors.New("HMAC verification failed")
	ErrGitlabTokenVerificationFailed = errors.New("gitlab token verification failed")
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
