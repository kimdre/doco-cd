package docker

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/opencontainers/go-digest"
)

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
	"cd.doco.job.",
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

// ProjectHash generates a SHA256 hash of the project configuration to be used for detecting changes in the project that may require a redeployment.
func ProjectHash(p *types.Project) (string, error) {
	pCopy := copyProject(p)

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
