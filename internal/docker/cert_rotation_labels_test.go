package docker

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

// generateTestCertPEM returns a self-signed PEM-encoded certificate with the given NotAfter time,
// for use in cert rotation label unit tests.
func generateTestCertPEM(t *testing.T, notAfter time.Time) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate test key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("failed to create test certificate: %v", err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestCertRotationLabelValues(t *testing.T) {
	t.Run("no external secrets returns not ok", func(t *testing.T) {
		deployConfig := &deploy.Config{}

		_, _, ok := certRotationLabelValues(deployConfig)
		if ok {
			t.Fatalf("expected ok=false when there are no external secrets")
		}
	})

	t.Run("non-cert external secrets are ignored", func(t *testing.T) {
		deployConfig := &deploy.Config{
			ExternalSecrets: map[string]secrettypes.ExternalSecretRef{
				"DB_PASSWORD": {LegacyRef: "kv:secret:db:password"},
			},
		}
		deployConfig.Internal.Environment = map[string]string{
			"DB_PASSWORD": "hunter2",
		}

		_, _, ok := certRotationLabelValues(deployConfig)
		if ok {
			t.Fatalf("expected ok=false when no external secrets are certificates")
		}
	})

	t.Run("rotatable pki-role cert sets expiry and rotatable=true", func(t *testing.T) {
		expiry := time.Now().Add(48 * time.Hour).Truncate(time.Second)
		certPEM := generateTestCertPEM(t, expiry)

		deployConfig := &deploy.Config{
			ExternalSecrets: map[string]secrettypes.ExternalSecretRef{
				"CERT": {LegacyRef: "pki-role:pki:my-role:app.example.com"},
			},
		}
		deployConfig.Internal.Environment = map[string]string{
			"CERT":     certPEM,
			"CERT_KEY": "not-a-certificate",
		}

		gotExpiry, gotRotatable, ok := certRotationLabelValues(deployConfig)
		if !ok {
			t.Fatalf("expected ok=true when a cert-bearing secret is present")
		}

		if gotRotatable != "true" {
			t.Errorf("expected rotatable=true, got %q", gotRotatable)
		}

		wantExpiry := expiry.UTC().Format(time.RFC3339)
		if gotExpiry != wantExpiry {
			t.Errorf("expected expiry %q, got %q", wantExpiry, gotExpiry)
		}
	})

	t.Run("read-only pki cert sets rotatable=false", func(t *testing.T) {
		expiry := time.Now().Add(24 * time.Hour).Truncate(time.Second)
		certPEM := generateTestCertPEM(t, expiry)

		deployConfig := &deploy.Config{
			ExternalSecrets: map[string]secrettypes.ExternalSecretRef{
				"CERT": {LegacyRef: "pki:pki:app.example.com"},
			},
		}
		deployConfig.Internal.Environment = map[string]string{
			"CERT": certPEM,
		}

		_, gotRotatable, ok := certRotationLabelValues(deployConfig)
		if !ok {
			t.Fatalf("expected ok=true when a cert-bearing secret is present")
		}

		if gotRotatable != "false" {
			t.Errorf("expected rotatable=false for a read-only pki ref, got %q", gotRotatable)
		}
	})

	t.Run("earliest expiry wins across multiple cert secrets", func(t *testing.T) {
		earlier := time.Now().Add(12 * time.Hour).Truncate(time.Second)
		later := time.Now().Add(72 * time.Hour).Truncate(time.Second)

		deployConfig := &deploy.Config{
			ExternalSecrets: map[string]secrettypes.ExternalSecretRef{
				"CERT_A": {LegacyRef: "pki-role:pki:role-a:a.example.com"},
				"CERT_B": {LegacyRef: "pki-role:pki:role-b:b.example.com"},
			},
		}
		deployConfig.Internal.Environment = map[string]string{
			"CERT_A": generateTestCertPEM(t, later),
			"CERT_B": generateTestCertPEM(t, earlier),
		}

		gotExpiry, gotRotatable, ok := certRotationLabelValues(deployConfig)
		if !ok {
			t.Fatalf("expected ok=true")
		}

		if gotRotatable != "true" {
			t.Errorf("expected rotatable=true, got %q", gotRotatable)
		}

		wantExpiry := earlier.UTC().Format(time.RFC3339)
		if gotExpiry != wantExpiry {
			t.Errorf("expected earliest expiry %q, got %q", wantExpiry, gotExpiry)
		}
	})

	t.Run("mixed rotatable and read-only cert secrets sets rotatable=false", func(t *testing.T) {
		expiry := time.Now().Add(48 * time.Hour).Truncate(time.Second)

		deployConfig := &deploy.Config{
			ExternalSecrets: map[string]secrettypes.ExternalSecretRef{
				"CERT_ROTATABLE": {LegacyRef: "pki-role:pki:role-a:a.example.com"},
				"CERT_READONLY":  {LegacyRef: "pki:pki:b.example.com"},
			},
		}
		deployConfig.Internal.Environment = map[string]string{
			"CERT_ROTATABLE": generateTestCertPEM(t, expiry),
			"CERT_READONLY":  generateTestCertPEM(t, expiry),
		}

		_, gotRotatable, ok := certRotationLabelValues(deployConfig)
		if !ok {
			t.Fatalf("expected ok=true")
		}

		if gotRotatable != "false" {
			t.Errorf("expected rotatable=false when any cert secret is read-only, got %q", gotRotatable)
		}
	})
}

func TestApplyCertRotationLabels(t *testing.T) {
	t.Run("adds labels when a cert secret is present", func(t *testing.T) {
		expiry := time.Now().Add(48 * time.Hour).Truncate(time.Second)

		deployConfig := &deploy.Config{
			ExternalSecrets: map[string]secrettypes.ExternalSecretRef{
				"CERT": {LegacyRef: "pki-role:pki:my-role:app.example.com"},
			},
		}
		deployConfig.Internal.Environment = map[string]string{
			"CERT": generateTestCertPEM(t, expiry),
		}

		labels := map[string]string{}
		applyCertRotationLabels(labels, deployConfig)

		if labels[DocoCDLabels.Deployment.CertExpiry] == "" {
			t.Errorf("expected CertExpiry label to be set")
		}

		if labels[DocoCDLabels.Deployment.CertRotatable] != "true" {
			t.Errorf("expected CertRotatable label to be \"true\", got %q", labels[DocoCDLabels.Deployment.CertRotatable])
		}
	})

	t.Run("does not add labels when there are no cert secrets", func(t *testing.T) {
		deployConfig := &deploy.Config{}

		labels := map[string]string{}
		applyCertRotationLabels(labels, deployConfig)

		if len(labels) != 0 {
			t.Errorf("expected no labels to be added, got %v", labels)
		}
	})
}

func TestApplyCertRotationLabelsToService(t *testing.T) {
	expiry := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	certPEM := generateTestCertPEM(t, expiry)
	unrelatedValue := "bar"

	deployConfig := &deploy.Config{
		ExternalSecrets: map[string]secrettypes.ExternalSecretRef{
			"CERT": {LegacyRef: "pki-role:pki:my-role:app.example.com"},
		},
	}
	deployConfig.Internal.Environment = map[string]string{"CERT": certPEM}

	project := &types.Project{
		Services: types.Services{
			"uses-cert": {
				Name:        "uses-cert",
				Environment: types.MappingWithEquals{"CERT": &certPEM},
			},
			"unrelated": {
				Name:        "unrelated",
				Environment: types.MappingWithEquals{"FOO": &unrelatedValue},
			},
		},
	}

	certLabels := map[string]string{}
	applyCertRotationLabelsToService(certLabels, project.Services["uses-cert"], project, deployConfig)

	if certLabels[DocoCDLabels.Deployment.CertRotatable] != "true" {
		t.Fatalf("expected certificate-consuming service to be labeled, got %v", certLabels)
	}

	var certState []deployedRotatableCertState
	if err := json.Unmarshal([]byte(certLabels[DocoCDLabels.Deployment.CertState]), &certState); err != nil {
		t.Fatalf("expected cert state label to contain valid JSON, got %q: %v", certLabels[DocoCDLabels.Deployment.CertState], err)
	}

	if len(certState) != 1 {
		t.Fatalf("expected exactly one deployed cert state entry, got %v", certState)
	}

	if certState[0].Ref != "pki-role:pki:my-role:app.example.com" {
		t.Fatalf("expected cert state ref to match the external secret, got %v", certState)
	}

	wantSerial, err := certificateSerial(certPEM)
	if err != nil {
		t.Fatalf("failed to parse test certificate serial: %v", err)
	}

	if certState[0].Serial != wantSerial {
		t.Fatalf("expected cert state serial %q, got %q", wantSerial, certState[0].Serial)
	}

	unrelatedLabels := map[string]string{}
	applyCertRotationLabelsToService(unrelatedLabels, project.Services["unrelated"], project, deployConfig)

	if len(unrelatedLabels) != 0 {
		t.Fatalf("expected unrelated service to have no cert rotation labels, got %v", unrelatedLabels)
	}
}

func TestCertificateSerial(t *testing.T) {
	expiry := time.Now().Add(24 * time.Hour)
	certPEM := generateTestCertPEM(t, expiry)

	serial, err := certificateSerial(certPEM)
	if err != nil {
		t.Fatalf("expected certificate serial parsing to succeed: %v", err)
	}

	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatal("expected PEM certificate block")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("expected generated certificate to parse: %v", err)
	}

	want := hex.EncodeToString(cert.SerialNumber.Bytes())
	if serial != want {
		t.Fatalf("expected serial %q, got %q", want, serial)
	}
}
