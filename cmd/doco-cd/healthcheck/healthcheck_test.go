package healthcheck

import (
	"context"
	"testing"
)

func TestCheck(t *testing.T) {
	// Start a local HTTP server for testing
	testCases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{
			name:    "Valid URL",
			url:     "https://httpbin.org/status/200",
			wantErr: false,
		},
		{
			name:    "Invalid URL",
			url:     "http://invalid.url",
			wantErr: true,
		},
		{
			name:    "Non-200 Status",
			url:     "https://httpbin.org/status/500",
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := Check(context.Background(), tc.url)
			if (err != nil) != tc.wantErr {
				t.Errorf("Check(%q) error = %v, wantErr %v", tc.url, err, tc.wantErr)
			}
		})
	}
}
