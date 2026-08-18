package docker

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/opencontainers/go-digest"
)

// isScaledToZero reports whether a service is configured to run 0 replicas
// ("scale: 0" / "deploy.replicas: 0"). Services scaled to zero never produce
// a container (or, in Swarm mode, never produce running tasks for
// replicated/replicated-job services), so their absence from the deployed
// state is expected and must not be treated as drift.
func isScaledToZero(svc types.ServiceConfig) bool {
	return svc.GetScale() == 0
}

// deepCopy recursively copies src into dst using reflection.
func deepCopy(dst, src reflect.Value) {
	switch src.Kind() {
	case reflect.Pointer:
		if src.IsNil() {
			return
		}

		dst.Set(reflect.New(src.Elem().Type()))
		deepCopy(dst.Elem(), src.Elem())
	case reflect.Struct:
		for i := 0; i < src.NumField(); i++ {
			field := src.Type().Field(i)
			if field.PkgPath != "" { // unexported field
				continue
			}

			deepCopy(dst.Field(i), src.Field(i))
		}
	case reflect.Slice:
		if src.IsNil() {
			return
		}

		dst.Set(reflect.MakeSlice(src.Type(), src.Len(), src.Cap()))

		for i := 0; i < src.Len(); i++ {
			deepCopy(dst.Index(i), src.Index(i))
		}
	case reflect.Map:
		if src.IsNil() {
			return
		}

		dst.Set(reflect.MakeMapWithSize(src.Type(), src.Len()))

		for _, key := range src.MapKeys() {
			val := reflect.New(src.MapIndex(key).Type()).Elem()
			deepCopy(val, src.MapIndex(key))
			dst.SetMapIndex(key, val)
		}
	default:
		dst.Set(src)
	}
}

// copyProject creates a deep copy of the given project struct by marshaling it to JSON and unmarshalling it back to a new struct.
// This is necessary because some fields in the compose types are pointers, and we want to avoid modifying the original struct when adding labels.
func copyProject(orig *types.Project) *types.Project {
	if orig == nil {
		return nil
	}

	clone := &types.Project{}
	deepCopy(reflect.ValueOf(clone).Elem(), reflect.ValueOf(orig).Elem())

	return clone
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

	pCopy := copyProject(p)

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

// ProjectHash generates a SHA256 hash of the project configuration to be used for detecting changes in the project that may require a redeployment.
func ProjectHash(p *types.Project) (string, error) {
	pCopy := copyProject(p)

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

	return digest.SHA256.FromBytes(b).Encoded(), nil
}
