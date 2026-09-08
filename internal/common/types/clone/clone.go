// Package clone provides helpers for copying reference types.
package clone

import (
	"reflect"
	"sync"
)

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
// It is a no-op if either dst or src is nil.
//
// Unexported struct fields cannot be traversed by reflection, so they are
// copied bitwise, exactly as a plain `*dst = *src` assignment would. Types
// whose unexported fields hold references (for example a struct wrapping a
// private slice) therefore still share that storage with src.
//
// Cyclic references are not supported and cause unbounded recursion.
func Deep[T any](dst, src *T) {
	if dst == nil || src == nil {
		return
	}

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
		// Copy the whole struct first so unexported fields, which reflection
		// cannot read or set individually, keep their values instead of being
		// silently zeroed. Only exported fields that actually hold references
		// need to be deep-copied over it below.
		dst.Set(src)

		srcType := src.Type()

		for i := range srcType.NumField() {
			field := srcType.Field(i)
			if field.PkgPath != "" || !needsDeepCopy(field.Type) { // unexported or value-only
				continue
			}

			deepValue(dst.Field(i), src.Field(i))
		}
	case reflect.Slice:
		if src.IsNil() {
			return
		}

		dst.Set(reflect.MakeSlice(src.Type(), src.Len(), src.Cap()))

		if !needsDeepCopy(src.Type().Elem()) {
			reflect.Copy(dst, src)

			return
		}

		for i := range src.Len() {
			deepValue(dst.Index(i), src.Index(i))
		}
	case reflect.Map:
		if src.IsNil() {
			return
		}

		dst.Set(reflect.MakeMapWithSize(src.Type(), src.Len()))

		mapType := src.Type()
		deepElem := needsDeepCopy(mapType.Elem())
		iter := src.MapRange()

		for iter.Next() {
			if !deepElem {
				dst.SetMapIndex(iter.Key(), iter.Value())

				continue
			}

			val := reflect.New(mapType.Elem()).Elem()
			deepValue(val, iter.Value())
			dst.SetMapIndex(iter.Key(), val)
		}
	default:
		dst.Set(src)
	}
}

// deepCopyCache memoizes needsDeepCopy results, which are constant per type.
var deepCopyCache sync.Map // reflect.Type -> bool

// needsDeepCopy reports whether values of t can hold references that must be
// recreated to make a copy independent of its source. Value-only types are
// already fully copied by a bitwise assignment, so they can be skipped.
//
// Map keys are not inspected: Go forbids maps, slices and functions as key
// types, and the remaining reference-capable key types (pointers, interfaces
// and arrays of them) are compared by identity, so cloning them would change
// lookup semantics.
func needsDeepCopy(t reflect.Type) bool {
	if cached, ok := deepCopyCache.Load(t); ok {
		return cached.(bool)
	}

	var needed bool

	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
		needed = true
	case reflect.Array:
		needed = needsDeepCopy(t.Elem())
	case reflect.Struct:
		// Unexported fields are copied bitwise and never traversed, so they
		// cannot make a deep copy necessary.
		for i := range t.NumField() {
			if field := t.Field(i); field.PkgPath == "" && needsDeepCopy(field.Type) {
				needed = true

				break
			}
		}
	default:
		needed = false
	}

	deepCopyCache.Store(t, needed)

	return needed
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
