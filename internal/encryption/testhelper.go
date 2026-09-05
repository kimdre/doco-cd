package encryption

// testhelper.go contains helper functions for testing encryption functionality.

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/getsops/sops/v3/age"
)

// sopsAgeKeyPath is the path to the test age key file used for testing encryption.
var sopsAgeKeyPath = func() string {
	_, filename, _, _ := runtime.Caller(0) // Get the directory of the current file and construct the path to the test age key file.

	return filepath.Join(filepath.Dir(filename), "testdata", "age-key.txt")
}()

// SetupAgeKeyEnvVar sets the SOPS_AGE_KEY environment variable for testing purposes.
func SetupAgeKeyEnvVar(t *testing.T) {
	t.Helper()
	t.Logf("Set %s environment variable to %s", age.SopsAgeKeyFileEnv, sopsAgeKeyPath)
	t.Setenv(age.SopsAgeKeyFileEnv, sopsAgeKeyPath)
}
