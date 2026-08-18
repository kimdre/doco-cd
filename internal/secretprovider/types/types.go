package secrettypes

type ResolvedSecrets map[string]string

// PKIRoleKeySuffix is the suffix appended to the env var name to hold the
// private key for a pki-role external secret (e.g. CERT → CERT_KEY).
const PKIRoleKeySuffix = "_KEY"
