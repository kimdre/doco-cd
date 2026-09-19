//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// registryHost strips the repository:tag portion off an OCI reference,
// leaving the host[:port] PushOCIArtifact pushed it under.
func registryHost(ref string) string {
	host, _, _ := strings.Cut(ref, "/")

	return host
}

// ociComposeIncludePlaceholder marks where the oci-compose-include scenario's
// fixture compose file expects the freshly pushed registry reference, since
// that reference is only known once the scenario's Docker network exists.
const ociComposeIncludePlaceholder = "__OCI_COMPOSE_INCLUDE_REF__"

// ociSourceArtifactFiles is the doco-cd v1 OCI artifact TestOCISourceDeployment
// builds and pushes to the suite's own test registry, replacing what used to
// be an externally published ghcr.io fixture. It deploys project
// "test-deploy" with two services: "test", which pulls in "included" via a
// local compose include, and ships a SOPS-encrypted env file encrypted to the
// same throwaway age key as the rest of the suite (sopsAgeKey).
var ociSourceArtifactFiles = map[string]string{
	".doco-cd.yaml": "name: test-deploy\nworking_dir: .\n",
	"compose.yaml": `include:
  - ./included.compose.yaml

services:
  test:
    image: alpine:3.22
    command: ["sleep", "600"]
    env_file:
      - .env
`,
	"included.compose.yaml": `services:
  included:
    image: alpine:3.22
    command: ["sleep", "600"]
`,
	// Same content as internal/encryption/testdata/encrypted.env, encrypted
	// to sopsAgeKey; decrypts to THIS_IS_ENCRYPTED=yes.
	".env": `THIS_IS_ENCRYPTED=ENC[AES256_GCM,data:bBwL,iv:qJtIth7EjmkNbvhlx2M8sngavde04qO9FR2OEVm+EgU=,tag:XFgb/SmNo2jH5ocqu0YgRQ==,type:str]
sops_age__list_0__map_enc=-----BEGIN AGE ENCRYPTED FILE-----\nYWdlLWVuY3J5cHRpb24ub3JnL3YxCi0+IFgyNTUxOSBCQ21OcUxoc3R2TWgvd3Ew\nWWNmZjFKeERrUDNkVksxK3luV29hZVRIR0NvCm4vb3VMKzNHL0gxNVk1VE1tMG5s\nZ0NFVWNFcUcrTjBrOGdsenIxSFoxM3MKLS0tIGNVd3d2amxwS2ZOTDJ0eWVEZnVM\nZy9rdUtaeXRsNmpVSGI0SHdjTTVwbVUKuBn450SIpc294UGstHSLh5VY/660M9CT\nlO6ChZb+vAdUuAW5hssymLY4qCNmP7W8cgui3ClDolx55b/aWpJmTw==\n-----END AGE ENCRYPTED FILE-----\n
sops_age__list_0__map_recipient=age1g3lclrkw2j0fkvq8a9jgvhglu408378tmwj84uv05qkhvax2cdrqcwqvmx
sops_lastmodified=2025-06-28T18:23:43Z
sops_mac=ENC[AES256_GCM,data:3nsfA/0G1AM9i0o7kmQHec4PKbUaV+5KVx4bcLCiHjoPVG6QHIXihhunSr0dUeywOVJ/K+jDS72zHfTw7TAyLzYd5OCH99vP8c6ksKrT3M6NQuEyhQEtJEHDQBqbLaOJghbJ9oP0NYnBcQIv1VXNhL8pnA+Dh+o0eSwPkEapKlE=,iv:2g8pa73ztA0hnTpV1gdEsmnr1L3e85RxryMsH9hHwdM=,tag:47KlQgzwq8EAK0ekl9T8KQ==,type:str]
sops_unencrypted_suffix=_unencrypted
sops_version=3.9.0
`,
}

// ociComposeIncludeArtifactFiles is the compose fragment TestOCIComposeInclude
// pulls in via "include: oci://...", replacing what used to be an externally
// published ghcr.io fixture.
var ociComposeIncludeArtifactFiles = map[string]string{
	"compose.yaml": `services:
  oci-test:
    image: alpine:3.22
    command: ["sleep", "600"]
`,
}

// TestOCISourceDeployment deploys straight from an OCI artifact rather than a
// git repository: the poll configuration names an OCI source, so the daemon
// resolves the artifact digest, pulls it, decrypts its SOPS-encrypted env file
// and deploys the stack it describes.
func TestOCISourceDeployment(t *testing.T) {
	t.Parallel()

	const (
		stack   = "test-deploy"
		service = "test"
	)

	h := NewHarness(t, "oci-source")
	h.TrackStack(stack)

	ref := h.PushOCIArtifact("test", ociSourceArtifactFiles)
	h.SetPollDocument("- source: oci\n  url: " + ref + "\n  interval: 10s\n")
	// The artifact ships a SOPS-encrypted env file, encrypted to the same age
	// key the other scenarios use.
	h.SetEnv("SOPS_AGE_KEY", sopsAgeKey)
	h.Start()

	h.WaitFor(3*time.Minute, "container deployed from the OCI artifact", func() bool {
		return h.ContainerID(stack, service) != ""
	})

	// The artifact's compose file uses a local include, so the included
	// service has to come up as well.
	h.WaitFor(time.Minute, "included service from the OCI artifact", func() bool {
		return h.ContainerID(stack, "included") != ""
	})
}

// TestOCIComposeInclude checks a git-sourced stack whose compose file pulls in
// a second compose file published as an OCI artifact via "include: oci://...".
func TestOCIComposeInclude(t *testing.T) {
	t.Parallel()

	const (
		stack         = "e2e-oci-compose-include"
		localService  = "app"
		remoteService = "oci-test"
	)

	h := NewHarness(t, "oci-compose-include")
	h.SetPreCommitHook(func() {
		ref := h.PushComposeOCIArtifact("compose-oci", ociComposeIncludeArtifactFiles)
		h.ReplaceInWorktree("deploy/compose.yaml", ociComposeIncludePlaceholder, ref)
		// Unlike doco-cd's own OCI client, the compose "include: oci://..."
		// loader doesn't auto-detect plain HTTP for RFC1918/loopback hosts,
		// so the test registry has to be listed explicitly.
		h.SetEnv("OCI_INSECURE_REGISTRIES", registryHost(ref))
	})
	h.Start()

	h.WaitFor(3*time.Minute, "local service of the including stack", func() bool {
		return h.ContainerID(stack, localService) != ""
	})

	h.WaitFor(time.Minute, "service included from the OCI artifact", func() bool {
		return h.ContainerID(stack, remoteService) != ""
	})
}
