// Package clone provides helpers for copying reference types.
package clone

import "reflect"

// New returns a deep copy of src in a newly allocated value, or nil if src is nil.
// It is a convenience wrapper around Deep for callers that don't already
// have a destination to copy into.
func New[T any](src *T) *T {
	if src == nil {
		return nil
	}

	dst := new(T)
	Deep(dst, src)

	return dst
}

// Deep creates an independent copy of src into dst using reflection.
// It recursively copies structs, pointers, slices, maps and interface values,
// so that no reference-type field of dst shares storage with src.
// Unexported struct fields are left at their zero value, matching Go's
// reflection limitations (they cannot be read or set via reflect).
func Deep[T any](dst, src *T) {
	deepValue(reflect.ValueOf(dst).Elem(), reflect.ValueOf(src).Elem())
}

// deepValue recursively copies src into dst. Both values must be addressable
// (or settable, in the case of dst) and of the same type.
func deepValue(dst, src reflect.Value) {
	switch src.Kind() {
	case reflect.Pointer:
		if src.IsNil() {
			return
		}

		dst.Set(reflect.New(src.Elem().Type()))
		deepValue(dst.Elem(), src.Elem())
	case reflect.Interface:
		if src.IsNil() {
			return
		}

		// Copy the concrete value held by the interface, so nested
		// maps/slices/pointers stored as `any` (e.g. map[string]any) are
		// deep-copied too, not just the interface reference.
		elem := src.Elem()
		copied := reflect.New(elem.Type()).Elem()
		deepValue(copied, elem)
		dst.Set(copied)
	case reflect.Struct:
		for i := 0; i < src.NumField(); i++ {
			if src.Type().Field(i).PkgPath != "" { // unexported field
				continue
			}

			deepValue(dst.Field(i), src.Field(i))
		}
	case reflect.Slice:
		if src.IsNil() {
			return
		}

		dst.Set(reflect.MakeSlice(src.Type(), src.Len(), src.Cap()))

		for i := 0; i < src.Len(); i++ {
			deepValue(dst.Index(i), src.Index(i))
		}
	case reflect.Map:
		if src.IsNil() {
			return
		}

		dst.Set(reflect.MakeMapWithSize(src.Type(), src.Len()))

		for _, key := range src.MapKeys() {
			val := reflect.New(src.MapIndex(key).Type()).Elem()
			deepValue(val, src.MapIndex(key))
			dst.SetMapIndex(key, val)
		}
	default:
		dst.Set(src)
	}
}

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
