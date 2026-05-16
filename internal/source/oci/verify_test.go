package oci

import (
	"context"
	"errors"
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
)

func TestVerifyWithCosign_DisabledPolicySkipsVerification(t *testing.T) {
	t.Parallel()

	err := VerifyWithCosign(context.Background(), "ghcr.io/example/app:main", "sha256:deadbeef", config.OciTrustPolicy{}, config.OciTrustPolicyOverride{})
	if err != nil {
		t.Fatalf("expected nil error for disabled policy, got %v", err)
	}
}

func TestVerifyWithCosign_EmptyDigestFails(t *testing.T) {
	t.Parallel()

	policy := config.OciTrustPolicy{Enabled: true}

	err := VerifyWithCosign(context.Background(), "ghcr.io/example/app:main", "", policy, config.OciTrustPolicyOverride{})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}

	if !errors.Is(err, ErrVerificationFailed) {
		t.Fatalf("expected ErrVerificationFailed, got %v", err)
	}
}

func TestVerifyWithCosign_NoTrustRulesFails(t *testing.T) {
	t.Parallel()

	policy := config.OciTrustPolicy{Enabled: true}

	err := VerifyWithCosign(context.Background(), "ghcr.io/example/app:main", "sha256:deadbeef", policy, config.OciTrustPolicyOverride{})
	if !errors.Is(err, ErrNoTrustRules) {
		t.Fatalf("expected ErrNoTrustRules, got %v", err)
	}
}

func TestToCosignIdentity_MapsSubjectRegexp(t *testing.T) {
	t.Parallel()

	identity := config.OciKeylessIdentity{
		Issuer:        " https://token.actions.githubusercontent.com ",
		Subject:       " ",
		SubjectRegexp: " ^https://github.com/myorg/myrepo/.+@refs/heads/main$ ",
	}

	got := toCosignIdentity(identity)
	if got.Issuer != "https://token.actions.githubusercontent.com" {
		t.Fatalf("unexpected issuer: %q", got.Issuer)
	}

	if got.Subject != "" {
		t.Fatalf("expected empty subject, got %q", got.Subject)
	}

	if got.SubjectRegExp != "^https://github.com/myorg/myrepo/.+@refs/heads/main$" {
		t.Fatalf("unexpected subject regexp: %q", got.SubjectRegExp)
	}
}
