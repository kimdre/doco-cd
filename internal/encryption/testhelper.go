package encryption

// testhelper.go contains helper functions for testing encryption functionality.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

var sopsAgeKeyPath = func() string {
	_, filename, _, _ := runtime.Caller(0)

	return filepath.Join(filepath.Dir(filename), "testdata", "age-key.txt")
}()

const sopsAgeKeyEnv = "SOPS_AGE_KEY_FILE"

// SetupAgeKeyEnvVar sets the SOPS_AGE_KEY environment variable for testing purposes.
func SetupAgeKeyEnvVar(t *testing.T) {
	t.Helper()

	t.Logf("Set %s environment variable to %s", sopsAgeKeyEnv, sopsAgeKeyPath)

	err := os.Setenv(sopsAgeKeyEnv, sopsAgeKeyPath)
	if err != nil {
		t.Fatalf("Failed to set %s environment variable: %v", sopsAgeKeyEnv, err)
	}

	t.Cleanup(func() {
		// Clean up the environment variable after the test
		t.Logf("Unset %s environment variable after test", sopsAgeKeyEnv)

		err = os.Unsetenv(sopsAgeKeyEnv)
		if err != nil {
			t.Errorf("Failed to unset %s environment variable: %v", sopsAgeKeyEnv, err)
		}
	})
}
