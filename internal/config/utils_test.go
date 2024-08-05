package config

import "testing"

func TestFromYAML(t *testing.T) {
	// Test cases
	tests := []struct {
		name    string
		file    string
		wantErr bool
	}{
		{
			name:    "valid yaml",
			file:    "testdata/valid.yaml",
			wantErr: false,
		},
		{
			name:    "invalid yaml",
			file:    "testdata/invalid.yaml",
			wantErr: true,
		},
		{
			name:    "non-existent file",
			file:    "testdata/non-existent.yaml",
			wantErr: true,
		},
	}

	// Run tests
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := FromYAML(tt.file)
			if (err != nil) != tt.wantErr {
				t.Errorf("FromYAML() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
