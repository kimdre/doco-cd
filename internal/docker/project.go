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
	case reflect.Ptr:
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

// ProjectHash generates a SHA256 hash of the project configuration to be used for detecting changes in the project that may require a redeployment.
func ProjectHash(p *types.Project) (string, error) {
	pCopy := copyProject(p)

	// Set all dynamic values to a constant value to avoid unnecessary changes in the hash when these values change but the actual configuration does not.
	for name, cfg := range pCopy.Services {
		for l := range cfg.Labels {
			if strings.HasPrefix(l, "cd.doco.") || strings.HasPrefix(l, "com.docker.compose.") {
				delete(cfg.Labels, l)
			}
		}

		pCopy.Services[name] = cfg
	}

	for v, cfg := range pCopy.Volumes {
		for l := range cfg.Labels {
			if strings.HasPrefix(l, "cd.doco.") || strings.HasPrefix(l, "com.docker.compose.") {
				delete(cfg.Labels, l)
			}
		}

		pCopy.Volumes[v] = cfg
	}

	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("failed to marshal project for hashing: %w", err)
	}

	return digest.SHA256.FromBytes(b).Encoded(), nil
}
