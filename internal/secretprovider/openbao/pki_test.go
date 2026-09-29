package openbao

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
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

	cert, certPEM := generateTestCAWithKey(t, commonName, key, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))

	return cert, certPEM, key
}

// generateTestCAWithKey creates a self-signed CA certificate for key, valid from notBefore to
// notAfter, and returns both its parsed form and its PEM encoding. Calling it repeatedly with the
// same key mimics an issuer reissued with its existing key.
func generateTestCAWithKey(t *testing.T, commonName string, key *ecdsa.PrivateKey, notBefore, notAfter time.Time) (*x509.Certificate, string) {
	t.Helper()

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
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

	return cert, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
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
	now := time.Now()

	rootACert, rootAPEM, rootAKey := generateTestCA(t, "root-a")
	rootBCert, rootBPEM, _ := generateTestCA(t, "root-b")
	leaf := mustParsePEMCertificate(t, generateTestLeaf(t, "leaf.example.com", rootACert, rootAKey))

	issuerA := pkiIssuer{Certificate: rootACert, PEM: rootAPEM, CAChain: []string{rootAPEM}}
	issuerB := pkiIssuer{Certificate: rootBCert, PEM: rootBPEM, CAChain: []string{rootBPEM}}

	// reissuedA returns issuer A reissued with its existing key and the given validity window, so it
	// verifies the same leaf certificates as issuer A does.
	reissuedA := func(notBefore, notAfter time.Time) pkiIssuer {
		cert, certPEM := generateTestCAWithKey(t, "root-a", rootAKey, notBefore, notAfter)

		return pkiIssuer{Certificate: cert, PEM: certPEM, CAChain: []string{certPEM}}
	}

	expiredA := reissuedA(now.Add(-72*time.Hour), now.Add(-48*time.Hour))
	recentlyExpiredA := reissuedA(now.Add(-48*time.Hour), now.Add(-24*time.Hour))
	notYetValidA := reissuedA(now.Add(time.Hour), now.Add(96*time.Hour))
	longerLivedA := reissuedA(now.Add(-time.Hour), now.Add(48*time.Hour))

	testCases := []struct {
		name     string
		issuers  []pkiIssuer
		expected []string
	}{
		{
			name:     "matches the issuer that actually signed the leaf, ignoring unrelated issuers",
			issuers:  []pkiIssuer{issuerB, issuerA},
			expected: []string{rootAPEM},
		},
		{
			name:     "falls back to the issuer certificate itself when it has no ca_chain",
			issuers:  []pkiIssuer{{Certificate: rootACert, PEM: rootAPEM}},
			expected: []string{rootAPEM},
		},
		{
			name:     "skips issuers without a parsed certificate",
			issuers:  []pkiIssuer{{PEM: "not a pem"}, issuerA},
			expected: []string{rootAPEM},
		},
		{
			name:     "prefers a currently valid issuer over expired and not yet valid ones sharing its key",
			issuers:  []pkiIssuer{expiredA, notYetValidA, issuerA, recentlyExpiredA},
			expected: issuerA.CAChain,
		},
		{
			name:     "prefers the valid issuer expiring last among valid ones sharing its key",
			issuers:  []pkiIssuer{issuerA, longerLivedA},
			expected: longerLivedA.CAChain,
		},
		{
			name:     "prefers the issuer expiring last when none sharing its key is currently valid",
			issuers:  []pkiIssuer{recentlyExpiredA, expiredA},
			expected: recentlyExpiredA.CAChain,
		},
		{
			name:    "no match when none of the issuers signed the leaf",
			issuers: []pkiIssuer{issuerB},
		},
		{
			name: "no issuers configured",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			chain, ok := matchingIssuerChain(leaf, tc.issuers, now)

			if tc.expected == nil {
				if ok {
					t.Fatalf("expected no matching issuer, got chain %v", chain)
				}

				return
			}

			if !ok {
				t.Fatal("expected a matching issuer to be found")
			}

			if !reflect.DeepEqual(chain, tc.expected) {
				t.Errorf("expected chain %v, got %v", tc.expected, chain)
			}
		})
	}
}

// mockPKIMount serves a minimal OpenBao PKI mount at "pki/" and records how often each endpoint is
// hit. The authenticated issuer endpoint (pki/issuer/<id>) always denies access, mimicking a
// least-privilege token, while the unauthenticated pki/issuer/<id>/json endpoint serves issuers.
type mockPKIMount struct {
	certs          map[string]mockPKILeaf // serial -> issued leaf certificate
	issuers        map[string]string      // issuer id -> issuer certificate PEM
	failingIssuers map[string]bool        // issuer ids whose /json read fails
	noIssuers      bool                   // listing issuers returns 404, like a pre multi-issuer mount
	defaultChain   string                 // chain served by pki/cert/ca_chain

	mu   sync.Mutex
	hits map[string]int
}

type mockPKILeaf struct {
	commonName string
	pem        string
}

// provider starts the mock OpenBao server and returns a Provider talking to it.
func (m *mockPKIMount) provider(t *testing.T) *Provider {
	t.Helper()

	m.hits = make(map[string]int)

	server := httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(server.Close)

	provider, err := NewProvider(t.Context(), server.URL, "token")
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	return provider
}

func (m *mockPKIMount) hitCount(endpoint string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.hits[endpoint]
}

func (m *mockPKIMount) handle(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/")

	method := r.Method
	if method == "LIST" || r.URL.Query().Get("list") == "true" {
		method = "LIST"
	}

	m.mu.Lock()
	m.hits[method+" "+path]++
	m.mu.Unlock()

	denied := func() {
		http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)
	}

	switch {
	case method == "LIST" && path == "pki/certs/detailed":
		keys := make([]string, 0, len(m.certs))
		keyInfo := make(map[string]any, len(m.certs))

		for serial, leaf := range m.certs {
			keys = append(keys, serial)
			keyInfo[serial] = map[string]any{"common_name": leaf.commonName}
		}

		writeMockPKIData(w, map[string]any{"keys": keys, "key_info": keyInfo})
	case method == "LIST" && path == "pki/issuers":
		if m.noIssuers {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[]}`))

			return
		}

		keys := make([]string, 0, len(m.issuers))
		for id := range m.issuers {
			keys = append(keys, id)
		}

		writeMockPKIData(w, map[string]any{"keys": keys})
	case path == "pki/cert/ca_chain":
		// Like OpenBao, the ca_chain pseudo-serial returns the chain as a single string.
		writeMockPKIData(w, map[string]any{"certificate": m.defaultChain, "ca_chain": m.defaultChain})
	case strings.HasPrefix(path, "pki/cert/"):
		leaf, ok := m.certs[strings.TrimPrefix(path, "pki/cert/")]
		if !ok {
			denied()
			return
		}

		// Like OpenBao, reading an issued certificate returns no chain.
		writeMockPKIData(w, map[string]any{"certificate": leaf.pem})
	case strings.HasPrefix(path, "pki/issuer/") && strings.HasSuffix(path, "/json"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "pki/issuer/"), "/json")

		certPEM, ok := m.issuers[id]
		if !ok || m.failingIssuers[id] {
			denied()
			return
		}

		writeMockPKIData(w, map[string]any{"certificate": certPEM, "ca_chain": []string{certPEM}, "issuer_id": id})
	default:
		denied()
	}
}

func writeMockPKIData(w http.ResponseWriter, data map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func mustParsePEMCertificate(t *testing.T, value string) *x509.Certificate {
	t.Helper()

	cert, err := parsePEMCertificate(value)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}

	return cert
}

// pemBundle joins PEM certificates the way joinPEMChain does, for building expected values.
func pemBundle(certs ...string) string {
	trimmed := make([]string, 0, len(certs))
	for _, cert := range certs {
		trimmed = append(trimmed, strings.TrimSpace(cert))
	}

	return strings.Join(trimmed, "\n")
}

func TestResolveSecretReferences_PKIChainOfNonDefaultSigningIssuer(t *testing.T) {
	t.Parallel()

	issuerACert, issuerAPEM, issuerAKey := generateTestCA(t, "issuer-a")
	_, issuerBPEM, _ := generateTestCA(t, "issuer-b")
	leafPEM := generateTestLeaf(t, "app.example.com", issuerACert, issuerAKey)

	// Issuer B is the mount's default, but issuer A signed the certificate. The token may not read
	// the authenticated issuer endpoint, so the chain must come from the unauthenticated one.
	mount := &mockPKIMount{
		certs:        map[string]mockPKILeaf{"01": {commonName: "app.example.com", pem: leafPEM}},
		issuers:      map[string]string{"a": issuerAPEM, "b": issuerBPEM},
		defaultChain: issuerBPEM,
	}
	provider := mount.provider(t)

	resolved, err := provider.ResolveSecretReferences(t.Context(), map[string]string{
		"CERT": "pki:pki:app.example.com", // #nosec G101
	})
	if err != nil {
		t.Fatalf("ResolveSecretReferences() error = %v", err)
	}

	if resolved["CERT"] != leafPEM {
		t.Errorf("expected CERT to hold the leaf certificate, got %q", resolved["CERT"])
	}

	if want := pemBundle(leafPEM, issuerAPEM); resolved["CERT_FULL"] != want {
		t.Errorf("expected CERT_FULL to hold the leaf followed by the signing issuer's chain\nwant %q\ngot  %q", want, resolved["CERT_FULL"])
	}

	if hits := mount.hitCount("GET pki/cert/ca_chain"); hits != 0 {
		t.Errorf("expected the default chain not to be read once the signing issuer is found, got %d reads", hits)
	}

	cert, fullChain, err := GetCertWithFullChain(t.Context(), provider.Client, "pki", "01")
	if err != nil {
		t.Fatalf("GetCertWithFullChain() error = %v", err)
	}

	if cert != leafPEM || fullChain != resolved["CERT_FULL"] {
		t.Errorf("expected GetCertWithFullChain to match ResolveSecretReferences, got cert %q and chain %q", cert, fullChain)
	}
}

func TestResolveSecretReferences_PKIChainFailsWhenIssuerCannotBeRead(t *testing.T) {
	t.Parallel()

	issuerACert, issuerAPEM, issuerAKey := generateTestCA(t, "issuer-a")
	_, issuerBPEM, _ := generateTestCA(t, "issuer-b")
	leafPEM := generateTestLeaf(t, "app.example.com", issuerACert, issuerAKey)

	// The signing issuer A can't be read and the readable issuer B didn't sign the certificate, so
	// the default chain may belong to a different CA and must not be used.
	mount := &mockPKIMount{
		certs:          map[string]mockPKILeaf{"01": {commonName: "app.example.com", pem: leafPEM}},
		issuers:        map[string]string{"a": issuerAPEM, "b": issuerBPEM},
		failingIssuers: map[string]bool{"a": true},
		defaultChain:   issuerBPEM,
	}
	provider := mount.provider(t)

	_, err := provider.ResolveSecretReferences(t.Context(), map[string]string{
		"CERT": "pki:pki:app.example.com", // #nosec G101
	})
	if err == nil {
		t.Fatal("expected an error when the signing issuer can't be determined")
	}

	if !strings.Contains(err.Error(), "issuer a") {
		t.Errorf("expected the error to identify the unreadable issuer, got: %v", err)
	}

	// Reading a certificate happens before any pki-role issuance, so the failure stays retryable.
	if errors.Is(err, secrettypes.ErrNotRetryable) {
		t.Errorf("expected a retryable error, got: %v", err)
	}

	if hits := mount.hitCount("GET pki/cert/ca_chain"); hits != 0 {
		t.Errorf("expected the default chain not to be used as a guess, got %d reads", hits)
	}
}

func TestResolveSecretReferences_PKIChainFallsBackToDefaultChain(t *testing.T) {
	t.Parallel()

	issuerACert, issuerAPEM, issuerAKey := generateTestCA(t, "issuer-a")
	_, issuerBPEM, _ := generateTestCA(t, "issuer-b")
	leafPEM := generateTestLeaf(t, "app.example.com", issuerACert, issuerAKey)

	testCases := []struct {
		name  string
		mount *mockPKIMount
	}{
		{
			name: "no configured issuer signed the certificate",
			mount: &mockPKIMount{
				issuers:      map[string]string{"b": issuerBPEM},
				defaultChain: issuerAPEM,
			},
		},
		{
			name: "mount lists no issuers",
			mount: &mockPKIMount{
				noIssuers:    true,
				defaultChain: issuerAPEM,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tc.mount.certs = map[string]mockPKILeaf{"01": {commonName: "app.example.com", pem: leafPEM}}
			provider := tc.mount.provider(t)

			resolved, err := provider.ResolveSecretReferences(t.Context(), map[string]string{
				"CERT": "pki:pki:app.example.com", // #nosec G101
			})
			if err != nil {
				t.Fatalf("ResolveSecretReferences() error = %v", err)
			}

			if want := pemBundle(leafPEM, issuerAPEM); resolved["CERT_FULL"] != want {
				t.Errorf("expected CERT_FULL to fall back to the mount's default chain\nwant %q\ngot  %q", want, resolved["CERT_FULL"])
			}

			if hits := tc.mount.hitCount("GET pki/cert/ca_chain"); hits != 1 {
				t.Errorf("expected the default chain to be read once, got %d reads", hits)
			}
		})
	}
}

func TestResolveSecretReferences_PKIIssuersLoadedOncePerMount(t *testing.T) {
	t.Parallel()

	issuerACert, issuerAPEM, issuerAKey := generateTestCA(t, "issuer-a")
	_, issuerBPEM, _ := generateTestCA(t, "issuer-b")
	firstLeafPEM := generateTestLeaf(t, "first.example.com", issuerACert, issuerAKey)
	secondLeafPEM := generateTestLeaf(t, "second.example.com", issuerACert, issuerAKey)

	mount := &mockPKIMount{
		certs: map[string]mockPKILeaf{
			"01": {commonName: "first.example.com", pem: firstLeafPEM},
			"02": {commonName: "second.example.com", pem: secondLeafPEM},
		},
		issuers:      map[string]string{"a": issuerAPEM, "b": issuerBPEM},
		defaultChain: issuerBPEM,
	}
	provider := mount.provider(t)

	resolved, err := provider.ResolveSecretReferences(t.Context(), map[string]string{
		"FIRST":  "pki:pki:first.example.com",  // #nosec G101
		"SECOND": "pki:pki:second.example.com", // #nosec G101
		"THIRD":  "pki:pki:first.example.com",  // #nosec G101
	})
	if err != nil {
		t.Fatalf("ResolveSecretReferences() error = %v", err)
	}

	for envVar, leafPEM := range map[string]string{"FIRST": firstLeafPEM, "SECOND": secondLeafPEM, "THIRD": firstLeafPEM} {
		if want := pemBundle(leafPEM, issuerAPEM); resolved[envVar+"_FULL"] != want {
			t.Errorf("expected %s_FULL to hold the leaf followed by the signing issuer's chain\nwant %q\ngot  %q", envVar, want, resolved[envVar+"_FULL"])
		}
	}

	for _, endpoint := range []string{"LIST pki/issuers", "GET pki/issuer/a/json", "GET pki/issuer/b/json"} {
		if hits := mount.hitCount(endpoint); hits != 1 {
			t.Errorf("expected %s to be requested once for all references on the mount, got %d", endpoint, hits)
		}
	}
}
