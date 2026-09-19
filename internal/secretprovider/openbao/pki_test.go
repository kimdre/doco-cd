package openbao

import (
	"reflect"
	"testing"
)

const (
	testLeafPEM   = "-----BEGIN CERTIFICATE-----\nleaf\n-----END CERTIFICATE-----"
	testIssuerPEM = "-----BEGIN CERTIFICATE-----\nissuer\n-----END CERTIFICATE-----"
	testRootPEM   = "-----BEGIN CERTIFICATE-----\nroot\n-----END CERTIFICATE-----"
)

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
