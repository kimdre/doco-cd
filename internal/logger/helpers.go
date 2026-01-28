package logger

import (
	"log/slog"
	"reflect"
	"strings"
)

// BuildLogValue returns a slog.Value for any value, excluding fields by name or dot-path.
// Examples:
//   - "Deployments" excludes the top-level field
//   - "Deployments.Internal" excludes Internal inside each element of Deployments
//
// Matches both Go field names and yaml tag names at each level.
func BuildLogValue(v any, ignore ...string) slog.Value {
	ignoreSet := make(map[string]struct{}, len(ignore))
	for _, p := range ignore {
		ignoreSet[p] = struct{}{}
	}

	return slog.AnyValue(buildPlain(reflect.ValueOf(v), "", ignoreSet))
}

// BuildSliceLogValue maps any slice/array to a slog.Value, applying nested ignore paths.
func BuildSliceLogValue(slice any, ignore ...string) slog.Value {
	ignoreSet := make(map[string]struct{}, len(ignore))
	for _, p := range ignore {
		ignoreSet[p] = struct{}{}
	}

	return slog.AnyValue(buildPlain(reflect.ValueOf(slice), "", ignoreSet))
}

func buildPlain(rv reflect.Value, prefix string, ignoreSet map[string]struct{}) any {
	if !rv.IsValid() {
		return nil
	}

	// Unwrap interfaces/pointers
	for rv.Kind() == reflect.Interface || rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}

		rv = rv.Elem()
	}

	switch rv.Kind() {
	case reflect.Struct:
		rt := rv.Type()
		out := make(map[string]any, rt.NumField())

		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if f.PkgPath != "" { // unexported
				continue
			}

			goName := f.Name
			yamlName := yamlKey(f)

			key := goName
			if yamlName != "" {
				key = yamlName
			}

			goPath := joinPath(prefix, goName)
			yamlPath := joinPath(prefix, key)

			if _, ok := ignoreSet[goPath]; ok {
				continue
			}

			if _, ok := ignoreSet[yamlPath]; ok {
				continue
			}

			out[key] = buildPlain(rv.Field(i), goPath, ignoreSet)
		}

		return out

	case reflect.Slice, reflect.Array:
		n := rv.Len()

		out := make([]any, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, buildPlain(rv.Index(i), prefix, ignoreSet))
		}

		return out

	case reflect.Map:
		out := make(map[string]any, rv.Len())

		iter := rv.MapRange()
		for iter.Next() {
			k := iter.Key()
			if k.Kind() == reflect.String {
				out[k.String()] = buildPlain(iter.Value(), prefix, ignoreSet)
			}
		}

		return out

	default:
		if rv.CanInterface() {
			return rv.Interface()
		}

		return nil
	}
}

func yamlKey(f reflect.StructField) string {
	tag := f.Tag.Get("yaml")
	if tag == "" {
		return ""
	}

	parts := strings.Split(tag, ",")
	if parts[0] == "" || parts[0] == "-" {
		return ""
	}

	return parts[0]
}

func joinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}

	return prefix + "." + name
}
