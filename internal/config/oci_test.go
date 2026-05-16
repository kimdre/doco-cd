package config

import "testing"

func TestNormalizeOciTrustPolicy_TrimsSubjectRegexp(t *testing.T) {
	t.Parallel()

	p := OciTrustPolicy{
		Enabled: true,
		KeylessIdentities: []OciKeylessIdentity{{
			Issuer:        " https://token.actions.githubusercontent.com ",
			Subject:       " https://github.com/example/repo/.github/workflows/build.yml@refs/heads/main ",
			SubjectRegexp: " ^https://github.com/example/repo/.+@refs/heads/main$ ",
		}},
	}

	normalized := NormalizeOciTrustPolicy(p)
	id := normalized.KeylessIdentities[0]

	if id.Issuer != "https://token.actions.githubusercontent.com" {
		t.Fatalf("unexpected issuer: %q", id.Issuer)
	}

	if id.Subject != "https://github.com/example/repo/.github/workflows/build.yml@refs/heads/main" {
		t.Fatalf("unexpected subject: %q", id.Subject)
	}

	if id.SubjectRegexp != "^https://github.com/example/repo/.+@refs/heads/main$" {
		t.Fatalf("unexpected subject_regexp: %q", id.SubjectRegexp)
	}
}
