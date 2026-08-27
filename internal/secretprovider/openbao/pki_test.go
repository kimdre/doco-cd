package openbao

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"reflect"
	"testing"
	"time"
)

const (
	testLeafPEM   = "-----BEGIN CERTIFICATE-----\nleaf\n-----END CERTIFICATE-----"
	testIssuerPEM = "-----BEGIN CERTIFICATE-----\nissuer\n-----END CERTIFICATE-----"
	testRootPEM   = "-----BEGIN CERTIFICATE-----\nroot\n-----END CERTIFICATE-----"
)

// generateTestCA creates a self-signed CA certificate and returns both its parsed form and its
// PEM encoding, along with the private key so callers can sign a leaf with it.
func generateTestCA(t *testing.T, commonName string) (*x509.Certificate, string, *ecdsa.PrivateKey) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}

	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))

	return cert, certPEM, key
}

// generateTestLeaf creates a certificate signed by the given CA and returns its PEM encoding.
func generateTestLeaf(t *testing.T, commonName string, ca *x509.Certificate, caKey *ecdsa.PrivateKey) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf certificate: %v", err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestJoinPEMChain(t *testing.T) {
	testCases := []struct {
		name     string
		leaf     string
		chain    []string
		expected string
	}{
		{
			name:     "No chain returns the leaf only",
			leaf:     testLeafPEM,
			chain:    nil,
			expected: testLeafPEM,
		},
		{
			name:     "Leaf is followed by the issuing chain",
			leaf:     testLeafPEM,
			chain:    []string{testIssuerPEM, testRootPEM},
			expected: testLeafPEM + "\n" + testIssuerPEM + "\n" + testRootPEM,
		},
		{
			name:     "Chain entry duplicating the leaf is dropped",
			leaf:     testLeafPEM,
			chain:    []string{testLeafPEM, testRootPEM},
			expected: testLeafPEM + "\n" + testRootPEM,
		},
		{
			name:     "Empty and whitespace-only entries are skipped",
			leaf:     "\n" + testLeafPEM + "\n",
			chain:    []string{"", "   ", testRootPEM},
			expected: testLeafPEM + "\n" + testRootPEM,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinPEMChain(tc.leaf, tc.chain); got != tc.expected {
				t.Errorf("Expected %q but got %q", tc.expected, got)
			}
		})
	}
}

func TestParseCAChain(t *testing.T) {
	testCases := []struct {
		name     string
		data     map[string]any
		expected []string
	}{
		{
			name:     "ca_chain returned as a list of strings",
			data:     map[string]any{"ca_chain": []string{testIssuerPEM, testRootPEM}},
			expected: []string{testIssuerPEM, testRootPEM},
		},
		{
			name:     "ca_chain returned as a JSON decoded list",
			data:     map[string]any{"ca_chain": []any{testIssuerPEM, testRootPEM}},
			expected: []string{testIssuerPEM, testRootPEM},
		},
		{
			name:     "Falls back to issuing_ca when no chain is present",
			data:     map[string]any{"issuing_ca": testRootPEM},
			expected: []string{testRootPEM},
		},
		{
			name:     "Falls back to issuing_ca when the chain is empty",
			data:     map[string]any{"ca_chain": []any{}, "issuing_ca": testRootPEM},
			expected: []string{testRootPEM},
		},
		{
			name:     "No chain information available",
			data:     map[string]any{"certificate": testLeafPEM},
			expected: nil,
		},
		{
			name:     "Blank issuing_ca is ignored",
			data:     map[string]any{"issuing_ca": "  "},
			expected: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseCAChain(tc.data); !reflect.DeepEqual(got, tc.expected) {
				t.Errorf("Expected %v but got %v", tc.expected, got)
			}
		})
	}
}

func TestIssuedCertificateFullChain(t *testing.T) {
	issued := IssuedCertificate{
		Certificate: testLeafPEM,
		CAChain:     []string{testRootPEM},
	}

	expected := testLeafPEM + "\n" + testRootPEM
	if got := issued.FullChain(); got != expected {
		t.Errorf("Expected %q but got %q", expected, got)
	}

	// The certificate itself must stay leaf-only, since it is what the private key pairs with.
	if issued.Certificate != testLeafPEM {
		t.Errorf("Expected the leaf certificate but got %q", issued.Certificate)
	}

	// Without a CA chain the bundle degrades to the leaf.
	noChain := IssuedCertificate{Certificate: testLeafPEM}
	if got := noChain.FullChain(); got != testLeafPEM {
		t.Errorf("Expected the leaf certificate but got %q", got)
	}
}

func TestMatchingIssuerChain(t *testing.T) {
	rootACert, rootAPEM, rootAKey := generateTestCA(t, "root-a")
	_, rootBPEM, _ := generateTestCA(t, "root-b")
	leafPEM := generateTestLeaf(t, "leaf.example.com", rootACert, rootAKey)

	leaf, err := parsePEMCertificate(leafPEM)
	if err != nil {
		t.Fatalf("parse leaf certificate: %v", err)
	}

	t.Run("matches the issuer that actually signed the leaf, ignoring unrelated issuers", func(t *testing.T) {
		issuers := []map[string]any{
			{"certificate": rootBPEM, "ca_chain": []any{rootBPEM}},
			{"certificate": rootAPEM, "ca_chain": []any{rootAPEM}},
		}

		chain, ok := matchingIssuerChain(leaf, issuers)
		if !ok {
			t.Fatal("expected a matching issuer to be found")
		}

		if !reflect.DeepEqual(chain, []string{rootAPEM}) {
			t.Errorf("expected chain %v, got %v", []string{rootAPEM}, chain)
		}
	})

	t.Run("falls back to the issuer certificate itself when it has no ca_chain", func(t *testing.T) {
		issuers := []map[string]any{
			{"certificate": rootAPEM},
		}

		chain, ok := matchingIssuerChain(leaf, issuers)
		if !ok {
			t.Fatal("expected a matching issuer to be found")
		}

		if !reflect.DeepEqual(chain, []string{rootAPEM}) {
			t.Errorf("expected chain %v, got %v", []string{rootAPEM}, chain)
		}
	})

	t.Run("no match when none of the issuers signed the leaf", func(t *testing.T) {
		issuers := []map[string]any{
			{"certificate": rootBPEM, "ca_chain": []any{rootBPEM}},
		}

		if _, ok := matchingIssuerChain(leaf, issuers); ok {
			t.Fatal("expected no matching issuer")
		}
	})

	t.Run("skips issuers with an unparsable or missing certificate", func(t *testing.T) {
		issuers := []map[string]any{
			{"certificate": "not a pem"},
			{},
			{"certificate": rootAPEM, "ca_chain": []any{rootAPEM}},
		}

		chain, ok := matchingIssuerChain(leaf, issuers)
		if !ok {
			t.Fatal("expected a matching issuer to be found")
		}

		if !reflect.DeepEqual(chain, []string{rootAPEM}) {
			t.Errorf("expected chain %v, got %v", []string{rootAPEM}, chain)
		}
	})

	t.Run("no issuers configured", func(t *testing.T) {
		if _, ok := matchingIssuerChain(leaf, nil); ok {
			t.Fatal("expected no matching issuer")
		}
	})
}
