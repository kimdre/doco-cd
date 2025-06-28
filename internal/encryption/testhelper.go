package encryption

// testhelper.go contains helper functions for testing encryption functionality.

import (
	"os"
	"testing"
)

// SetupAgeKeyEnvVar sets the SOPS_AGE_KEY environment variable for testing purposes.
func SetupAgeKeyEnvVar(t *testing.T) {
	t.Helper()

	const envVarName = "SOPS_AGE_KEY"

	t.Logf("Set %s environment variable for testing", envVarName)

	err := os.Setenv(envVarName, "AGE-SECRET-KEY-1U2W28TTH2KSRD0K0J36U93S2C5UW4RXRYGGQ8NPGCDG7RKFCT5SQEKNGQK")
	if err != nil {
		t.Fatalf("Failed to set %s environment variable: %v", envVarName, err)
	}

	t.Cleanup(func() {
		// Clean up the environment variable after the test
		t.Logf("Unset %s environment variable after test", envVarName)

		err = os.Unsetenv(envVarName)
		if err != nil {
			t.Errorf("Failed to unset %s environment variable: %v", envVarName, err)
		}
	})
}
