package hook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr error
	}{
		{
			name:    "valid http and https",
			config:  Config{OnSuccess: []Webhook{{URL: "http://example.com/a"}}, OnFailure: []Webhook{{URL: "https://example.com/b"}}},
			wantErr: nil,
		},
		{
			name:    "empty config",
			config:  Config{},
			wantErr: nil,
		},
		{
			name:    "empty url",
			config:  Config{OnSuccess: []Webhook{{URL: "  "}}},
			wantErr: ErrEmptyURL,
		},
		{
			name:    "non-http scheme",
			config:  Config{OnFailure: []Webhook{{URL: "ftp://example.com"}}},
			wantErr: ErrInvalidURL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestSend(t *testing.T) {
	payload := Payload{Event: "success", Repository: "user/repo", Stack: "web", Revision: "main (abc1234)", JobID: "job-1", Images: []string{"nginx:1.27", "postgres:16"}}

	var (
		gotMethod string
		gotHeader string
		gotBody   Payload
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotHeader = r.Header.Get("X-Token")

		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hk := Webhook{URL: srv.URL, Headers: map[string]string{"X-Token": "secret"}}

	if err := Send(context.Background(), hk, payload); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST (default)", gotMethod)
	}

	if gotHeader != "secret" {
		t.Errorf("custom header = %q, want secret", gotHeader)
	}

	if !reflect.DeepEqual(gotBody, payload) {
		t.Errorf("body = %+v, want %+v", gotBody, payload)
	}
}

func TestSendCustomMethod(t *testing.T) {
	var gotMethod string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method

		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := Send(context.Background(), Webhook{URL: srv.URL, Method: http.MethodPut}, Payload{}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
}

func TestSendNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := Send(context.Background(), Webhook{URL: srv.URL}, Payload{})
	if !errors.Is(err, ErrHookFailed) {
		t.Fatalf("Send() error = %v, want ErrHookFailed", err)
	}
}

func TestSendConnectionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // server is now unreachable

	err := Send(context.Background(), Webhook{URL: url}, Payload{})
	if !errors.Is(err, ErrHookFailed) {
		t.Fatalf("Send() error = %v, want ErrHookFailed", err)
	}
}
