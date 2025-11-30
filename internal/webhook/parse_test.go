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
	webhookPath       = "/v1/webhook"
	testSecret        = "secret"
	githubPayloadFile = "testdata/github_payload.json"
	giteaPayloadFile  = "testdata/gitea_payload.json"
	gitlabPayloadFile = "testdata/gitlab_payload.json"
)

func TestParse(t *testing.T) {
	testCases := []struct {
		name          string
		filePath      string
		expectedError error
	}{
		{"Github Push Payload", githubPayloadFile, nil},
		{"Gitea Push Payload", giteaPayloadFile, nil},
		{"Gitlab Push Payload", gitlabPayloadFile, nil},
		{"Invalid Signature", githubPayloadFile, ErrHMACVerificationFailed},
		{"Missing Signature", githubPayloadFile, ErrMissingSecurityHeader},
		{"Invalid Gitlab Token", gitlabPayloadFile, ErrGitlabTokenVerificationFailed},
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
					r.Header.Set(ScmProviderHeaders[Github], "sha256="+GenerateHMAC(payload, testSecret))
				case "Gitea Push Payload":
					r.Header.Set(ScmProviderHeaders[Gitea], GenerateHMAC(payload, testSecret))
				case "Gitlab Push Payload":
					r.Header.Set(ScmProviderHeaders[Gitlab], testSecret)
				}
			} else {
				switch {
				case errors.Is(tc.expectedError, ErrHMACVerificationFailed):
					r.Header.Set(ScmProviderHeaders[Github], "sha256=invalid")
				case errors.Is(tc.expectedError, ErrMissingSecurityHeader):
					// do nothing
				case errors.Is(tc.expectedError, ErrGitlabTokenVerificationFailed):
					r.Header.Set(ScmProviderHeaders[Gitlab], "invalid")
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
