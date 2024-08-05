package config

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

func TestFromYAML(t *testing.T) {
	// Test cases
	tests := []struct {
		name          string
		file          string
		expectedError error
	}{
		{
			name:          "valid yaml",
			file:          "testdata/valid.yaml",
			expectedError: nil,
		},
		{
			name:          "invalid yaml",
			file:          "testdata/invalid.yaml",
			expectedError: fmt.Errorf("failed to unmarshal yaml"),
		},
		{
			name:          "non-existent file",
			file:          "testdata/non-existent.yaml",
			expectedError: os.ErrNotExist,
		},
	}

	// Run tests
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := FromYAML(tt.file)
			if tt.expectedError == nil {
				if err != nil {
					t.Errorf("Expected no error, got %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("Expected error %v, got nil", tt.expectedError)
				} else if errors.Is(err, tt.expectedError) {
					t.Errorf("Expected error %v, got %v", tt.expectedError, err)
				}
			}
		})
	}
}
