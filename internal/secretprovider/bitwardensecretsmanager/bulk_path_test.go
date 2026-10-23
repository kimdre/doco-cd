package bitwardensecretsmanager

import (
	"errors"
	"testing"
)

func TestParseSecretPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path        string
		wantOrg     string
		wantProject string
		wantErr     bool
	}{
		{path: "org-1", wantOrg: "org-1"},
		{path: "org-1:proj-1", wantOrg: "org-1", wantProject: "proj-1"},
		{path: " org-1:proj-1 ", wantOrg: "org-1", wantProject: "proj-1"},
		{path: "org-1:", wantOrg: "org-1"},
		{path: "", wantErr: true},
		{path: ":proj-1", wantErr: true},
		{path: "org-1:proj-1:extra", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()

			org, project, err := parseSecretPath(tt.path)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidSecretPath) {
					t.Fatalf("expected ErrInvalidSecretPath, got %v", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if org != tt.wantOrg || project != tt.wantProject {
				t.Errorf("got (%q, %q), want (%q, %q)", org, project, tt.wantOrg, tt.wantProject)
			}
		})
	}
}
