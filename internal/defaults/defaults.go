/*
Package defaults applies struct-tag defaults to Go structs.

It is intended for config-style types that use `default` tags on fields.
Call Set with a non-nil pointer to a struct before validation or use.

Example:

	type Config struct {
		Source string `default:"git"`
		Delay  time.Duration `default:"30s"`
	}

	func main() {
		cfg := Config{}
		_ = defaults.Set(&cfg)
	}
*/
package defaults

import (
	"errors"
	"fmt"
	"reflect"
	"time"

	"go.yaml.in/yaml/v4"
)

var ErrInvalidTarget = errors.New("defaults: target must be a non-nil pointer to a struct")

// Set applies struct tag defaults to target.
func Set(target any) error {
	if target == nil {
		return ErrInvalidTarget
	}

	value := reflect.ValueOf(target)
	if value.Kind() != reflect.Pointer || value.IsNil() || value.Elem().Kind() != reflect.Struct {
		return ErrInvalidTarget
	}

	return apply(value.Elem())
}

// apply recursively walks value, applying default tags to every struct field
// it encounters. It descends through pointers, interfaces, slices, arrays,
// and maps to reach nested structs.
func apply(value reflect.Value) error {
	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			return nil
		}

		return apply(value.Elem())
	case reflect.Interface:
		if value.IsNil() {
			return nil
		}

		return apply(value.Elem())
	case reflect.Struct:
		return applyStruct(value)
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			if err := apply(value.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		if value.IsNil() {
			return nil
		}

		iter := value.MapRange()
		for iter.Next() {
			key := iter.Key()
			elem := iter.Value()

			if !elem.IsValid() {
				continue
			}

			if err := applyMapValue(value, key, elem); err != nil {
				return err
			}
		}
	}

	return nil
}

// applyStruct applies default tags to each exported field of value, then
// recurses into each field to reach any nested structs it contains.
func applyStruct(value reflect.Value) error {
	typ := value.Type()

	for i := 0; i < value.NumField(); i++ {
		fieldType := typ.Field(i)
		if fieldType.PkgPath != "" {
			continue
		}

		field := value.Field(i)
		if err := applyFieldDefault(field, fieldType); err != nil {
			return fmt.Errorf("%s: %w", fieldType.Name, err)
		}

		if err := apply(field); err != nil {
			return fmt.Errorf("%s: %w", fieldType.Name, err)
		}
	}

	return nil
}

// applyFieldDefault sets field to its parsed `default` tag value, but only
// when the tag is present, the field is settable, and the field still holds
// its zero value (i.e. it wasn't explicitly set by the caller).
func applyFieldDefault(field reflect.Value, fieldType reflect.StructField) error {
	defaultValue, ok := fieldType.Tag.Lookup("default")
	if !ok || !field.CanSet() || !field.IsZero() {
		return nil
	}

	parsed, err := parseDefault(field.Type(), defaultValue)
	if err != nil {
		return err
	}

	field.Set(parsed)

	return nil
}

// parseDefault converts the raw `default` tag string into a reflect.Value of
// targetType. time.Duration gets special handling so tags can use Go
// duration syntax (e.g. "30s") instead of a raw integer of nanoseconds.
// All other types are decoded as YAML, which covers scalars, slices, and
// maps expressed as tag strings (e.g. `default:"[\"a\",\"b\"]"`).
func parseDefault(targetType reflect.Type, raw string) (reflect.Value, error) {
	if targetType == reflect.TypeFor[time.Duration]() {
		if raw == "" {
			return reflect.Zero(targetType), nil
		}

		duration, err := time.ParseDuration(raw)
		if err != nil {
			return reflect.Value{}, err
		}

		value := reflect.New(targetType).Elem()
		value.SetInt(int64(duration))

		return value, nil
	}

	if raw == "" {
		return reflect.Zero(targetType), nil
	}

	value := reflect.New(targetType)
	if err := yaml.Unmarshal([]byte(raw), value.Interface()); err != nil {
		return reflect.Value{}, err
	}

	return value.Elem(), nil
}

// applyMapValue applies defaults to a single map entry.
//
// Map values obtained via MapRange are not addressable, so field mutations
// can't be written back to them directly. Pointer, Slice, and Map values are
// reference-like (they share underlying storage), so recursing into them
// in-place is sufficient. Struct and Array values are copied by value, so
// they must be cloned into an addressable copy, mutated, and written back
// with SetMapIndex.
func applyMapValue(container, key, elem reflect.Value) error {
	switch elem.Kind() {
	case reflect.Pointer, reflect.Interface:
		if elem.IsNil() {
			return nil
		}

		return apply(elem)
	case reflect.Slice, reflect.Map:
		return apply(elem)
	case reflect.Struct, reflect.Array:
		clone := reflect.New(elem.Type()).Elem()
		clone.Set(elem)

		if err := apply(clone); err != nil {
			return err
		}

		container.SetMapIndex(key, clone)

		return nil
	default:
		return nil
	}
}
