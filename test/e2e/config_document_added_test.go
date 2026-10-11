//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// sopsAgeKey decrypts the scenarios' encrypted fixtures. Same throwaway key as
// internal/encryption/testdata/age-key.txt, public key
// age1g3lclrkw2j0fkvq8a9jgvhglu408378tmwj84uv05qkhvax2cdrqcwqvmx.
const sopsAgeKey = "AGE-SECRET-KEY-1U2W28TTH2KSRD0K0J36U93S2C5UW4RXRYGGQ8NPGCDG7RKFCT5SQEKNGQK"

// TestDeployConfigGainsDocument appends plain and encrypted deployments without
// restarting the daemon. It checks both the first decryption and a later update
// after a decrypted artifact exists.
func TestDeployConfigGainsDocument(t *testing.T) {
	t.Parallel()

	const (
		first           = "e2e-config-document-added-first"
		plain           = "e2e-config-document-added-second"
		encryptedFirst  = "e2e-config-document-added-encrypted-first"
		encryptedSecond = "e2e-config-document-added-encrypted-second"
		service         = "app"
	)

	h := NewHarness(t, "config-document-added")
	h.SetEnv("SOPS_AGE_KEY", sopsAgeKey)
	h.Start()
	h.WaitFor(2*time.Minute, "initial stack container", func() bool {
		return h.ContainerID(first, service) != ""
	})
	h.WaitForLog(deployCompletedLog, 2*time.Minute)

	containers := map[string]string{first: h.ContainerID(first, service)}

	for _, phase := range []struct {
		dir   string
		added []string
	}{
		{dir: "update", added: []string{plain, encryptedFirst}},
		{dir: "encrypted-update", added: []string{encryptedSecond}},
	} {
		mark := h.LogMark()
		h.CopyScenarioDir(phase.dir)
		h.RepoPush("append deployment documents: " + phase.dir)

		for _, stack := range phase.added {
			h.WaitFor(2*time.Minute, stack+" deployed from appended document", func() bool {
				return h.ContainerID(stack, service) != ""
			})
		}

		h.WaitForLogAfter(`"msg":"job completed successfully"`, mark, 2*time.Minute)

		for stack, id := range containers {
			if got := h.ContainerID(stack, service); got != id {
				t.Fatalf("existing %s container changed: got %q, want %q", stack, got, id)
			}
		}

		for _, stack := range phase.added {
			containers[stack] = h.ContainerID(stack, service)
		}

		for _, stack := range []string{encryptedFirst, encryptedSecond} {
			if id, exists := containers[stack]; exists {
				plaintext := h.ExecOutput(id, "cat", "/secrets/secret.yaml")
				if !strings.Contains(plaintext, "e2e-decrypted-marker") || strings.Contains(plaintext, "ENC[") {
					t.Fatalf("%s secret was not decrypted: %q", stack, plaintext)
				}
			}
		}
	}
}
