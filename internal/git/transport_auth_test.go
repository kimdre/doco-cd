package git_test

import (
	"testing"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/git"
)

func TestHttpTokenAuth(t *testing.T) {
	testCases := []struct {
		name         string
		username     string
		token        string
		expectNil    bool
		expectedUser string
		expectedErr  error
	}{
		{
			name:         "Valid token defaults username",
			token:        "ghp_test123456",
			expectNil:    false,
			expectedUser: git.DefaultHTTPAuthUser,
			expectedErr:  nil,
		},
		{
			name:         "Custom username (e.g. GitLab deploy token)",
			username:     "gitlab+deploy-token-123",
			token:        "gldt_test123456",
			expectNil:    false,
			expectedUser: "gitlab+deploy-token-123",
			expectedErr:  nil,
		},
		{
			name:        "Empty token",
			token:       "",
			expectNil:   true,
			expectedErr: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			auth := git.HttpTokenAuth(tc.username, tc.token)

			if tc.expectNil && auth != nil {
				t.Fatal("Expected nil auth for empty token")
			}

			if !tc.expectNil && auth == nil {
				t.Fatal("Expected non-nil auth for valid token")
			}

			if auth == nil {
				return
			}

			if auth.Name() != "http-basic-auth" {
				t.Fatalf("Expected auth name 'http-basic-auth', got '%s'", auth.Name())
			}

			basicAuth, ok := auth.(*githttp.BasicAuth)
			if !ok {
				t.Fatalf("Expected *githttp.BasicAuth, got %T", auth)
			}

			if basicAuth.Username != tc.expectedUser {
				t.Fatalf("Expected username '%s', got '%s'", tc.expectedUser, basicAuth.Username)
			}
		})
	}
}

func TestSSHAuth(t *testing.T) {
	t.Parallel()

	const (
		encryptedKey = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAACmFlczI1Ni1jdHIAAAAGYmNyeXB0AAAAGAAAABA+Zz/91P
rp2u7NvTWBtLI0AAAAGAAAAAEAAAAzAAAAC3NzaC1lZDI1NTE5AAAAIFyEIiKcYAJl82Ga
40hVJoKO1qOvVfekORkGLSsKFnF7AAAAoBgOn6fvoLqNvcj0QMyuZTYVJEm9YXs8zNkG+9
suGsdNHOvMRQWLzq9VJiJUyOG29zayIQ4Q3pZlcoRINpUI9yl4/eFza7P4MEHDVBLF531K
X3nAnZomTg2czfus92AmR+3kYDWvBE1WkpieAaRfVTuBtNcB41rOAZMLQ001zhVF2qdb+D
+tvLTkrbIyLPEbZOBHuCH+mVgPefYCRXsB9Nw=
-----END OPENSSH PRIVATE KEY-----`
		encryptedKeyPassphrase = app.Name
		unencryptedKey         = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACCU6Sk58h0kd2bUvHHvyS1JQiLgBf6yKaIbpGlK8TEfVAAAAJgBQMSpAUDE
qQAAAAtzc2gtZWQyNTUxOQAAACCU6Sk58h0kd2bUvHHvyS1JQiLgBf6yKaIbpGlK8TEfVA
AAAEBBVspZHjWj6Np5szQQHB6w+1X3ZOatDcMmcnm1+R9J9pTpKTnyHSR3ZtS8ce/JLUlC
IuAF/rIpohukaUrxMR9UAAAADmtpbUBraW0tZmVkb3JhAQIDBAUGBw==
-----END OPENSSH PRIVATE KEY-----`
	)

	testCases := []struct {
		name        string
		privateKey  string
		passphrase  string
		expectedErr string
	}{
		{
			name:        "Encrypted ED25519 key",
			privateKey:  encryptedKey,
			passphrase:  encryptedKeyPassphrase,
			expectedErr: "",
		},
		{
			name:        "Missing passphrase for encrypted key",
			privateKey:  encryptedKey,
			passphrase:  "",
			expectedErr: "failed to create SSH public keys: bcrypt_pbkdf: empty password",
		},
		{
			name:        "Unencrypted ED25519 key",
			privateKey:  unencryptedKey,
			passphrase:  "",
			expectedErr: "",
		},
		{
			name:        "Unencrypted ED25519 key with passphrase",
			privateKey:  unencryptedKey,
			passphrase:  "test",
			expectedErr: "",
		},
		{
			name:        "Missing private key",
			privateKey:  "",
			passphrase:  "",
			expectedErr: "ssh URL requires SSH_PRIVATE_KEY to be set",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			auth, err := git.SSHAuth(tc.privateKey, tc.passphrase)
			if err != nil {
				if tc.expectedErr == "" {
					t.Fatalf("Expected no error, got %v", err)
				}

				if err.Error() == tc.expectedErr {
					return
				}

				t.Fatalf("Expected error %v, got %v", tc.expectedErr, err.Error())
			} else if tc.expectedErr != "" {
				t.Fatalf("Expected error %v, got none", tc.expectedErr)
			}

			if auth == nil {
				if tc.expectedErr != "auth empty" {
					t.Fatal("Expected auth to be non-nil")
				}
			}

			if auth.Name() != "ssh-public-keys" {
				t.Fatalf("Expected auth name 'ssh-public-keys', got '%s'", auth.Name())
			}
		})
	}
}
