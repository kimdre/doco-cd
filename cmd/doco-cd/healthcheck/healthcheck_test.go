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

	testCases := []struct {
		name    string
		url     string
		wantErr bool
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
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := Check(context.Background(), tc.url)
			if (err != nil) != tc.wantErr {
				t.Errorf("Check(%q) error = %v, wantErr %v", tc.url, err, tc.wantErr)
			}
		})
	}
}
