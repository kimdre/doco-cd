package oci

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/sigstore/sigstore/pkg/cryptoutils"

	"github.com/kimdre/doco-cd/internal/config"
)

func TestVerifyWithCosign_DisabledPolicySkipsVerification(t *testing.T) {
	t.Parallel()

	err := VerifyWithCosign(context.Background(), "ghcr.io/example/app:main", "sha256:deadbeef", config.OciTrustPolicy{}, config.OciTrustPolicyOverride{}, 1)
	if err != nil {
		t.Fatalf("expected nil error for disabled policy, got %v", err)
	}
}

func TestVerifyWithCosign_EmptyDigestFails(t *testing.T) {
	t.Parallel()

	policy := config.OciTrustPolicy{Enabled: true}

	err := VerifyWithCosign(context.Background(), "ghcr.io/example/app:main", "", policy, config.OciTrustPolicyOverride{}, 1)
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

	err := VerifyWithCosign(context.Background(), "ghcr.io/example/app:main", "sha256:deadbeef", policy, config.OciTrustPolicyOverride{}, 1)
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

func TestNormalizeVerifyMaxWorkers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   uint
		want int
	}{
		{name: "zero defaults to one", in: 0, want: 1},
		{name: "one stays one", in: 1, want: 1},
		{name: "middle unchanged", in: 4, want: 4},
		{name: "above max clamps", in: 99, want: 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeVerifyMaxWorkers(tt.in); got != tt.want {
				t.Fatalf("normalizeVerifyMaxWorkers(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestLoadPublicKeyVerifier(t *testing.T) {
	t.Parallel()

	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	edPublic, edPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	message := []byte("doco-cd")
	digest := sha256.Sum256(message)

	ecdsaSignature, err := ecdsa.SignASN1(rand.Reader, ecdsaKey, digest[:])
	if err != nil {
		t.Fatal(err)
	}

	testCases := []struct {
		name      string
		publicKey crypto.PublicKey
		signature []byte
	}{
		{name: "ecdsa", publicKey: ecdsaKey.Public(), signature: ecdsaSignature},
		{name: "ed25519", publicKey: edPublic, signature: ed25519.Sign(edPrivate, message)},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pemKey, err := cryptoutils.MarshalPublicKeyToPEM(tc.publicKey)
			if err != nil {
				t.Fatal(err)
			}

			// Trust policies often carry surrounding whitespace from YAML block scalars.
			verifier, err := loadPublicKeyVerifier("\n  " + string(pemKey) + "\n")
			if err != nil {
				t.Fatalf("loadPublicKeyVerifier() error = %v", err)
			}

			if err = verifier.VerifySignature(bytes.NewReader(tc.signature), bytes.NewReader(message)); err != nil {
				t.Errorf("VerifySignature() error = %v", err)
			}

			if err = verifier.VerifySignature(bytes.NewReader(tc.signature), bytes.NewReader([]byte("tampered"))); err == nil {
				t.Error("VerifySignature() accepted a signature for a different message")
			}
		})
	}

	if _, err = loadPublicKeyVerifier("not a public key"); err == nil {
		t.Error("loadPublicKeyVerifier() accepted an invalid key")
	}
}
