package commitstatus

// Exported wrappers for internal functions used in tests.

var (
	ParseHostAndScheme   = parseHostAndScheme
	PostGitHub           = postGitHub
	PostGitHubCompatible = postGitHubCompatible
	PostGitLab           = postGitLab
	ResolveProvider      = resolveProvider
)
