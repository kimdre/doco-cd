package git

import (
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"

	"github.com/kimdre/doco-cd/internal/git/ssh"
)

// GetAuthMethod determines the appropriate authentication method based on the URL and provided credentials.
func GetAuthMethod(url, privateKey, keyPassphrase, token string) (transport.AuthMethod, error) {
	if IsSSH(url) {
		return SSHAuth(privateKey, keyPassphrase)
	} else if token != "" {
		return HttpTokenAuth(token), nil
	}

	return nil, nil
}

// IsSSH checks if a given URL is an SSH URL.
func IsSSH(url string) bool {
	return strings.HasPrefix(url, "git@") || strings.HasPrefix(url, "ssh://")
}

// SSHAuth creates an SSH authentication method using the provided private key.
func SSHAuth(privateKey, keyPassphrase string) (transport.AuthMethod, error) {
	if strings.TrimSpace(privateKey) == "" {
		return nil, ErrSSHKeyRequired
	}

	auth, err := gitssh.NewPublicKeys(ssh.DefaultGitSSHUser, []byte(privateKey), keyPassphrase)
	if err != nil {
		return nil, fmt.Errorf("failed to create SSH public keys: %w", err)
	}

	// auth.HostKeyCallback = ssh2.InsecureIgnoreHostKey()

	return auth, nil
}

// HttpTokenAuth returns an AuthMethod for HTTP Basic Auth using a token.
func HttpTokenAuth(token string) transport.AuthMethod {
	if token == "" {
		return nil
	}

	return &githttp.BasicAuth{
		Username: "oauth2", // can be anything except an empty string
		Password: token,
	}
}
