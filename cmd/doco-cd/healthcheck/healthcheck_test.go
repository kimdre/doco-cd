package healthcheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheck(t *testing.T) {
	t.Parallel()

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ok.Close)

	notOk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(notOk.Close)

	okTLS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(okTLS.Close)

	testCases := []struct {
		name          string
		url           string
		skipTLSVerify bool
		wantErr       bool
	}{
		{
			name:    "Valid URL",
			url:     ok.URL,
			wantErr: false,
		},
		{
			name:    "Invalid URL",
			url:     "http://invalid.url",
			wantErr: true,
		},
		{
			name:    "Non-200 Status",
			url:     notOk.URL,
			wantErr: true,
		},
		{
			name:          "Valid TLS URL",
			url:           okTLS.URL,
			skipTLSVerify: true,
			wantErr:       false,
		},
		{
			name:    "TLS URL without verify skip fails",
			url:     okTLS.URL,
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := Check(context.Background(), tc.url, tc.skipTLSVerify)
			if (err != nil) != tc.wantErr {
				t.Errorf("Check(%q) error = %v, wantErr %v", tc.url, err, tc.wantErr)
			}
		})
	}
}

func TestTargetFromEnv(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		env     map[string]string
		want    Target
		wantErr bool
	}{
		{
			name: "defaults to port 80 without TLS",
			env:  map[string]string{},
			want: Target{URL: "http://localhost:80/v1/health"},
		},
		{
			name: "empty port uses default",
			env:  map[string]string{"HTTP_PORT": " "},
			want: Target{URL: "http://localhost:80/v1/health"},
		},
		{
			name: "custom port",
			env:  map[string]string{"HTTP_PORT": "8080"},
			want: Target{URL: "http://localhost:8080/v1/health"},
		},
		{
			name: "TLS when certificate and key are set",
			env: map[string]string{
				"HTTP_PORT":          "8443",
				"HTTP_TLS_CERT_FILE": " /certs/tls.crt ",
				"HTTP_TLS_KEY_FILE":  "/certs/tls.key",
			},
			want: Target{URL: "https://localhost:8443/v1/health", SkipTLSVerify: true},
		},
		{
			name: "blank TLS settings disable TLS",
			env:  map[string]string{"HTTP_TLS_CERT_FILE": " ", "HTTP_TLS_KEY_FILE": ""},
			want: Target{URL: "http://localhost:80/v1/health"},
		},
		{
			name:    "certificate without key",
			env:     map[string]string{"HTTP_TLS_CERT_FILE": "/certs/tls.crt"},
			wantErr: true,
		},
		{
			name:    "key without certificate",
			env:     map[string]string{"HTTP_TLS_KEY_FILE": "/certs/tls.key"},
			wantErr: true,
		},
		{
			name:    "non-numeric port",
			env:     map[string]string{"HTTP_PORT": "http"},
			wantErr: true,
		},
		{
			name:    "port zero",
			env:     map[string]string{"HTTP_PORT": "0"},
			wantErr: true,
		},
		{
			name:    "port out of range",
			env:     map[string]string{"HTTP_PORT": "65536"},
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			lookupEnv := func(key string) (string, bool) {
				value, ok := tc.env[key]
				return value, ok
			}

			got, err := TargetFromEnv(lookupEnv, "/v1/health")
			if (err != nil) != tc.wantErr {
				t.Fatalf("TargetFromEnv() error = %v, wantErr %v", err, tc.wantErr)
			}

			if got != tc.want {
				t.Errorf("TargetFromEnv() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
