package secrettypes

import "errors"

type ResolvedSecrets map[string]string

// PKIRoleKeySuffix is the suffix appended to the env var name to hold the
// private key for a pki-role external secret (e.g. CERT → CERT_KEY).
const PKIRoleKeySuffix = "_KEY"

// ErrNotRetryable marks a provider error that must not be retried as a whole, e.g. a failure
// after a non-idempotent write like certificate issuance, where a retry would replay it.
var ErrNotRetryable = errors.New("not retryable")
