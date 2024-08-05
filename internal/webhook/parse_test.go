package webhook

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

const (
	webhookPath = "/v1/webhook"
	testSecret  = "secret"
)

func TestParse(t *testing.T) {
	testCases := []struct {
		name          string
		filePath      string
		expectedError error
	}{
		{"Github Push Payload", "testdata/github_payload.json", nil},
		{"Gitea Push Payload", "testdata/gitea_payload.json", nil},
		{"Gitlab Push Payload", "testdata/gitlab_payload.json", nil},
		{"Invalid Signature", "testdata/github_payload.json", ErrHMACVerificationFailed},
		{"Missing Signature", "testdata/github_payload.json", ErrMissingSecurityHeader},
		{"Invalid Gitlab Token", "testdata/gitlab_payload.json", ErrGitlabTokenVerificationFailed},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := os.ReadFile(tc.filePath)
			if err != nil {
				t.Fatal(err)
			}

			minifiedPayload := new(bytes.Buffer)

			err = json.Compact(minifiedPayload, payload)
			if err != nil {
				t.Fatal(err)
			}

			r := httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(payload))

			if tc.expectedError == nil {
				switch tc.name {
				case "Github Push Payload":
					r.Header.Set(GithubSignatureHeader, "sha256="+GenerateHMAC(payload, testSecret))
				case "Gitea Push Payload":
					r.Header.Set(GiteaSignatureHeader, GenerateHMAC(payload, testSecret))
				case "Gitlab Push Payload":
					r.Header.Set(GitlabTokenHeader, testSecret)
				}
			} else {
				switch tc.expectedError {
				case ErrHMACVerificationFailed:
					r.Header.Set(GithubSignatureHeader, "sha256=invalid")
				case ErrMissingSecurityHeader:
					// do nothing
				case ErrGitlabTokenVerificationFailed:
					r.Header.Set(GitlabTokenHeader, "invalid")
				}
			}

			p, err := Parse(r, testSecret)
			if tc.expectedError == nil {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}

				if p.FullName != "kimdre/doco-cd" {
					t.Errorf("expected repository name to be kimdre/doco-cd, got %s", p.FullName)
				}
			} else if !errors.Is(err, tc.expectedError) {
				t.Errorf("expected error to be %v, got %v", tc.expectedError, err)
			}
		})
	}
}
