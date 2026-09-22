package docker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/opencontainers/go-digest"

	"github.com/kimdre/doco-cd/internal/common/types/clone"
)

// isScaledToZero reports whether a service is configured to run 0 replicas
// ("scale: 0" / "deploy.replicas: 0"). Services scaled to zero never produce
// a container (or, in Swarm mode, never produce running tasks for
// replicated/replicated-job services), so their absence from the deployed
// state is expected and must not be treated as drift.
func isScaledToZero(svc types.ServiceConfig) bool {
	return svc.GetScale() == 0
}

// behaviorLabelPrefixes lists the cd.doco.* label prefixes that configure behaviour and
// should therefore be included in the project hash for redeploy detection.
// All other cd.doco.* labels (metadata, timestamps, …) are excluded.
var behaviorLabelPrefixes = []string{
	jobLabelPrefix,
	"cd.doco.deployment.recreate.",
}

func shouldIgnoreLabelInProjectHash(label string) bool {
	if strings.HasPrefix(label, "com.docker.compose.") {
		return true
	}

	if !strings.HasPrefix(label, "cd.doco.") {
		return false
	}

	for _, prefix := range behaviorLabelPrefixes {
		if strings.HasPrefix(label, prefix) {
			return false
		}
	}

	return true
}

// WithNormalizedEnvValues returns a copy of the project where any service
// environment value or top-level config content matching a key in normMap is
// replaced with the corresponding placeholder. Use this to produce a stable
// project hash when secrets are re-issued on every resolution (e.g. pki-role certs),
// so the hash only changes when the ref itself changes.
func WithNormalizedEnvValues(p *types.Project, normMap map[string]string) *types.Project {
	if len(normMap) == 0 || p == nil {
		return p
	}

	pCopy := clone.New(p)

	for name, svc := range pCopy.Services {
		changed := false

		for envKey, envVal := range svc.Environment {
			if envVal == nil {
				continue
			}

			if placeholder, ok := normMap[*envVal]; ok {
				v := placeholder
				svc.Environment[envKey] = &v
				changed = true
			}
		}

		if changed {
			pCopy.Services[name] = svc
		}
	}

	for cfgName, cfg := range pCopy.Configs {
		if placeholder, ok := normMap[cfg.Content]; ok {
			cfg.Content = placeholder
			pCopy.Configs[cfgName] = cfg
		}
	}

	return pCopy
}

// sourceRootPlaceholder replaces the on-disk source root inside hashed project data.
//
// Compose resolves every relative path in a project file (bind mount sources, env_file,
// config/secret files, build contexts, …) against the directory the project was loaded
// from. That directory is revision scoped (…/artifacts/<revision>/…), so the resolved
// absolute paths change with every new commit even when the stack itself is untouched.
// Replacing the source root with a fixed token keeps the hash tied to the project's
// content instead of to the revision it happens to be published under.
const sourceRootPlaceholder = "/__doco_cd_source_root__"

// normalizeSourceRoot rewrites every occurrence of sourceRoot in the marshaled project
// to sourceRootPlaceholder. It is a no-op for an empty, relative or filesystem root path.
func normalizeSourceRoot(b []byte, sourceRoot string) []byte {
	root := strings.TrimSpace(sourceRoot)
	if root == "" {
		return b
	}

	root = filepath.Clean(root)
	if !filepath.IsAbs(root) || root == string(filepath.Separator) {
		return b
	}

	return bytes.ReplaceAll(b, []byte(root), []byte(sourceRootPlaceholder))
}

// ProjectHash generates a SHA256 hash of the project configuration to be used for detecting changes in the project that may require a redeployment.
//
// sourceRoot is the on-disk directory the project was loaded from. When set, absolute
// paths below it are normalized so the hash stays stable across revisions of the same
// unchanged stack.
func ProjectHash(p *types.Project, sourceRoot string) (string, error) {
	pCopy := clone.New(p)

	// Services scaled to zero never create containers, so they must not affect restart-time hash checks.
	for name, svc := range pCopy.Services {
		if isScaledToZero(svc) {
			delete(pCopy.Services, name)
		}
	}

	// Only behavior-configuring labels should impact redeploy decisions.
	for name := range pCopy.Services {
		svc := pCopy.Services[name]
		if svc.Labels != nil {
			for l := range svc.Labels {
				if shouldIgnoreLabelInProjectHash(l) {
					delete(svc.Labels, l)
				}
			}
		}

		pCopy.Services[name] = svc
	}

	for vol := range pCopy.Volumes {
		volCfg := pCopy.Volumes[vol]
		if volCfg.Labels != nil {
			for l := range volCfg.Labels {
				if shouldIgnoreLabelInProjectHash(l) {
					delete(volCfg.Labels, l)
				}
			}
		}

		pCopy.Volumes[vol] = volCfg
	}

	b, err := json.Marshal(pCopy)
	if err != nil {
		return "", fmt.Errorf("failed to marshal project for hashing: %w", err)
	}

	return digest.SHA256.FromBytes(normalizeSourceRoot(b, sourceRoot)).Encoded(), nil
}
