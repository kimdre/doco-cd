package git

import (
	"errors"
	"testing"
)

func TestValidateDomainPattern(t *testing.T) {
	t.Parallel()

	valid := []string{
		"github.com",
		"GitHub.com.",
		"*.example.com",
		"git_server.internal",
		"localhost",
		"127.0.0.1",
		"::1",
	}

	for _, pattern := range valid {
		if err := ValidateDomainPattern(pattern); err != nil {
			t.Errorf("ValidateDomainPattern(%q) = %v, want nil", pattern, err)
		}
	}

	invalid := []string{
		"",
		"  ",
		"*",
		"*.",
		"*.*.example.com",
		"example.*.com",
		"https://github.com",
		"github.com/org",
		"github.com:443",
		"user@github.com",
		"-bad.example.com",
		"bad..example.com",
	}

	for _, pattern := range invalid {
		if err := ValidateDomainPattern(pattern); !errors.Is(err, ErrInvalidAuthDomain) {
			t.Errorf("ValidateDomainPattern(%q) = %v, want ErrInvalidAuthDomain", pattern, err)
		}
	}
}

func TestScopedAuthConfig_HasHTTPCredential(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		entry ScopedAuthConfig
		want  bool
	}{
		{name: "domains only", entry: ScopedAuthConfig{Domains: []string{"github.com"}}},
		{name: "SSH only", entry: ScopedAuthConfig{SSHPrivateKey: "key"}},
		{name: "incomplete app", entry: ScopedAuthConfig{GitHubAppID: "1"}},
		{name: "token", entry: ScopedAuthConfig{GitAccessToken: "token"}, want: true},
		{name: "app", entry: ScopedAuthConfig{GitHubAppID: "1", GitHubAppPrivateKey: "key"}, want: true},
	}

	for _, tt := range tests {
		if got := tt.entry.HasHTTPCredential(); got != tt.want {
			t.Errorf("%s: HasHTTPCredential() = %v, want %v", tt.name, got, tt.want)
		}
	}
}
