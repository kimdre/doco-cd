package secrettypes

import "errors"

type ResolvedSecrets map[string]string

// PKIRoleRefPrefix starts an external secret reference that issues a new certificate from a PKI
// role on every resolution (e.g. pki-role:pki:web:app.example).
const PKIRoleRefPrefix = "pki-role:"

// PKIRoleKeySuffix is the suffix appended to the env var name to hold the
// private key for a pki-role external secret (e.g. CERT → CERT_KEY).
const PKIRoleKeySuffix = "_KEY"

// PKIFullChainSuffix is the suffix appended to the env var name to hold the full certificate
// chain (leaf certificate followed by its issuing CA chain) of a pki or pki-role external
// secret (e.g. CERT → CERT_FULL).
const PKIFullChainSuffix = "_FULL"

// ErrNotRetryable marks a provider error that must not be retried as a whole, e.g. a failure
// after a non-idempotent write like certificate issuance, where a retry would replay it.
var ErrNotRetryable = errors.New("not retryable")
