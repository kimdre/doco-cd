package graceful

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// generateSelfSignedCert writes a throwaway self-signed certificate/key pair
// for "localhost" into dir and returns the cert and key file paths.
func generateSelfSignedCert(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	certFile = filepath.Join(dir, "server.crt")
	keyFile = filepath.Join(dir, "server.key")

	certOut, err := os.Create(certFile) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatalf("failed to create cert file: %v", err)
	}

	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("failed to encode certificate: %v", err)
	}

	if err := certOut.Close(); err != nil {
		t.Fatalf("failed to close cert file: %v", err)
	}

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("failed to marshal private key: %v", err)
	}

	keyOut, err := os.Create(keyFile) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatalf("failed to create key file: %v", err)
	}

	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		t.Fatalf("failed to encode private key: %v", err)
	}

	if err := keyOut.Close(); err != nil {
		t.Fatalf("failed to close key file: %v", err)
	}

	return certFile, keyFile
}

// freeLocalAddr reserves an ephemeral local TCP port and returns its address.
func freeLocalAddr(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve local port: %v", err)
	}

	addr := l.Addr().String()

	if err := l.Close(); err != nil {
		t.Fatalf("failed to close listener: %v", err)
	}

	return addr
}

func TestGraceHttpServer_PlainHTTP(t *testing.T) {
	t.Parallel()

	addr := freeLocalAddr(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := NewHttpServer("plain", &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: time.Second}, "", "")

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)

	go func() {
		errCh <- srv.Serve(ctx)
	}()

	waitForServer(t, "http://"+addr+"/ok", nil)

	cancel()

	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("expected clean shutdown, got %v", err)
	}
}

func TestGraceHttpServer_TLS(t *testing.T) {
	t.Parallel()

	certFile, keyFile := generateSelfSignedCert(t, t.TempDir())
	addr := freeLocalAddr(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := NewHttpServer("tls", &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: time.Second}, certFile, keyFile)

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)

	go func() {
		errCh <- srv.Serve(ctx)
	}()

	tlsConfig := &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- self-signed test certificate
	waitForServer(t, "https://"+addr+"/ok", tlsConfig)

	cancel()

	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("expected clean shutdown, got %v", err)
	}
}

func TestGraceHttpServer_MismatchedTLSFilesFailsFast(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		tlsCertFile string
		tlsKeyFile  string
	}{
		{name: "cert without key", tlsCertFile: "/tmp/does-not-matter.crt"},
		{name: "key without cert", tlsKeyFile: "/tmp/does-not-matter.key"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := NewHttpServer("mismatched", &http.Server{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second}, tc.tlsCertFile, tc.tlsKeyFile)

			err := srv.Serve(context.Background())
			if err == nil {
				t.Fatal("expected error for mismatched TLS cert/key files")
			}

			if !strings.Contains(err.Error(), "must both be set") {
				t.Fatalf("expected descriptive TLS config error, got: %v", err)
			}
		})
	}
}

// waitForServer polls url until it responds successfully or the timeout elapses.
func waitForServer(t *testing.T, url string, tlsConfig *tls.Config) {
	t.Helper()

	client := &http.Client{Timeout: time.Second}
	if tlsConfig != nil {
		client.Transport = &http.Transport{TLSClientConfig: tlsConfig}
	}

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		resp, err := client.Get(url) // #nosec G107 -- test-controlled local URL
		if err == nil {
			_ = resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				return
			}
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("server at %s did not become ready in time", url)
}
