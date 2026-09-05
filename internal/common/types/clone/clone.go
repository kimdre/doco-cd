// Package clone provides helpers for copying reference types.
package clone

// Pointer returns an independent copy of value.
func Pointer[T any](value *T) *T {
	if value == nil {
		return nil
	}

	cloned := *value

	return &cloned
}

// StringAnyMap returns an independent copy of source, including nested maps
// and slices stored as values.
func StringAnyMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}

	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = Any(value)
	}

	return cloned
}

// Any copies maps and slices that can be represented by values decoded from
// YAML or JSON. Other values are returned unchanged.
func Any(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return StringAnyMap(value)
	case []any:
		cloned := make([]any, len(value))
		for i, item := range value {
			cloned[i] = Any(item)
		}

		return cloned
	default:
		return value
	}
}
