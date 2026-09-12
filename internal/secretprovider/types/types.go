package secrettypes

type ResolvedSecrets map[string]string

// PKIRoleKeySuffix is the suffix appended to the env var name to hold the
// private key for a pki-role external secret (e.g. CERT → CERT_KEY).
const PKIRoleKeySuffix = "_KEY"

// PKIFullChainSuffix is the suffix appended to the env var name to hold the full certificate
// chain (leaf certificate followed by its issuing CA chain) of a pki or pki-role external
// secret (e.g. CERT → CERT_FULL).
const PKIFullChainSuffix = "_FULL"
