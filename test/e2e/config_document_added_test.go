//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// sopsAgeKey decrypts the scenarios' encrypted fixtures. Same throwaway key as
// internal/encryption/testdata/age-key.txt, public key
// age1g3lclrkw2j0fkvq8a9jgvhglu408378tmwj84uv05qkhvax2cdrqcwqvmx.
const sopsAgeKey = "AGE-SECRET-KEY-1U2W28TTH2KSRD0K0J36U93S2C5UW4RXRYGGQ8NPGCDG7RKFCT5SQEKNGQK"

// TestDeployConfigGainsDocument verifies that a second YAML document appended
// to a deploy config the daemon is already polling gets deployed on the next
// poll, without restarting the daemon.
//
// The two sops cases are here because doco-cd decrypts SOPS files in place in
// the worktree, which leaves that worktree permanently dirty, and the checkout
// path skips the files it recorded as decrypted when it resets tracked files.
// Whether the repository already had a decrypted file before the document
// landed, or gains its first one in that same commit, changes what that skip
// list holds at the moment the config file itself has to be updated.
func TestDeployConfigGainsDocument(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		scenario string
		// decrypts marks a scenario whose added stack mounts a directory
		// holding a SOPS-encrypted file. Its service greps the plaintext
		// marker and exits when it is missing, so the stack only stays up if
		// doco-cd really decrypted the file in place.
		decrypts bool
	}{
		{
			name:     "plain",
			scenario: "config-document-added",
		},
		{
			// A decrypted file already exists when the document is added.
			name:     "sops_already_decrypted",
			scenario: "config-document-added-sops-existing",
			decrypts: true,
		},
		{
			// The repository's first encrypted file arrives in the same commit
			// as the document that reads it.
			name:     "sops_first_decryption",
			scenario: "config-document-added-sops-new",
			decrypts: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			const service = "app"

			firstStack := "e2e-" + tc.scenario + "-first"
			secondStack := "e2e-" + tc.scenario + "-second"

			h := NewHarness(t, tc.scenario)
			if tc.decrypts {
				h.SetEnv("SOPS_AGE_KEY", sopsAgeKey)
			}

			h.Start()

			h.WaitFor(2*time.Minute, "initial stack container", func() bool {
				return h.ContainerID(firstStack, service) != ""
			})

			firstID := h.ContainerID(firstStack, service)
			logMark := h.LogMark()

			// The new stack's working directory arrives in the same commit as
			// the new document, the way a stack is added to a deployments repo
			// in practice.
			h.CopyScenarioDir("update")
			h.RepoPush("add a second deploy config document")

			h.WaitFor(2*time.Minute, "stack from the appended document deployed", func() bool {
				return h.ContainerID(secondStack, service) != ""
			})
			h.WaitForLogAfter(`"msg":"job completed successfully"`, logMark, 2*time.Minute)

			if got := h.ContainerID(firstStack, service); got != firstID {
				t.Fatalf("existing stack container changed: got %q, want %q", got, firstID)
			}
		})
	}
}
