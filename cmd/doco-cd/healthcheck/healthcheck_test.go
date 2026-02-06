package healthcheck

import (
	"context"
	"testing"
	"time"
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

	const (
		maxRetries = 3
		retryDelay = 500 * time.Millisecond
	)

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			for attempt := 1; attempt <= maxRetries; attempt++ {
				err = Check(context.Background(), tc.url)
				if (err != nil) == tc.wantErr {
					break // Success or expected error
				}

				if attempt < maxRetries {
					t.Logf("Retrying (%d/%d) after error: %v", attempt, maxRetries, err)
					time.Sleep(retryDelay)
				}
			}

			if (err != nil) != tc.wantErr {
				t.Errorf("Check(%q) error = %v, wantErr %v", tc.url, err, tc.wantErr)
			}
		})
	}
}
