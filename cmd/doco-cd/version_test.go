package main

import (
	"testing"
)

func TestGetLatestAppReleaseVersion(t *testing.T) {
	t.Parallel()

	version, err := getLatestAppReleaseVersion()
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if version == "" {
		t.Fatal("expected a version string, got empty")
	}

	t.Logf("Latest version: %s", version)
}
