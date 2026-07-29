package validation

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	playgroundvalidator "github.com/go-playground/validator/v10"
)

type TextError struct {
	Err error
}

// Error returns the wrapped error text.
func (e TextError) Error() string {
	if e.Err == nil {
		return ""
	}

	return e.Err.Error()
}

// Unwrap exposes the wrapped error for errors.Is and errors.As.
func (e TextError) Unwrap() error {
	return e.Err
}

// Func adapts custom field validation to the tag+parameter shape used in struct tags.
type Func func(v any, param string) error

var (
	engine = playgroundvalidator.New(playgroundvalidator.WithRequiredStructEnabled())

	customFuncs   = map[string]Func{}
	customFuncsMu sync.RWMutex
)

func init() {
	engine.RegisterAlias("nonzero", "required")
}

// RegisterValidationFunc registers a custom validation tag for use in struct field tags.
func RegisterValidationFunc(tag string, fn Func) error {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return errors.New("validation tag cannot be empty")
	}

	if fn == nil {
		return fmt.Errorf("validation function for %q cannot be nil", tag)
	}

	customFuncsMu.Lock()
	defer customFuncsMu.Unlock()

	if _, exists := customFuncs[tag]; exists {
		return fmt.Errorf("validation %q already registered", tag)
	}

	// Register with the engine first so a failure does not leave behind an entry
	// that makes the tag look registered and blocks any later attempt.
	if err := registerEngineTag(tag); err != nil {
		return err
	}

	customFuncs[tag] = fn

	return nil
}

// registerEngineTag registers tag as a no-op engine rule, since the actual check
// runs in this package. The engine panics on restricted tags, so that is
// converted into an error to keep registration failures recoverable.
func registerEngineTag(tag string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("failed to register validation %q: %v", tag, r)
		}
	}()

	return engine.RegisterValidation(tag, func(_ playgroundvalidator.FieldLevel) bool {
		return true
	})
}

// lookupValidationFunc returns the validator registered for tag, if any.
func lookupValidationFunc(tag string) (Func, bool) {
	customFuncsMu.RLock()
	defer customFuncsMu.RUnlock()

	fn, ok := customFuncs[tag]

	return fn, ok
}

// Validate applies validator rules and this package's custom tag handlers.
func Validate(v any) error {
	return validateValue(reflect.ValueOf(v))
}

// validateValue traverses structs and collections so custom tags run recursively.
func validateValue(value reflect.Value) error {
	value = dereference(value)
	if !value.IsValid() {
		return nil
	}

	switch value.Kind() {
	case reflect.Struct:
		if err := engine.Struct(value.Interface()); err != nil {
			return err
		}

		return validateCustomTags(value, "")
	case reflect.Slice, reflect.Array:
		for i := range value.Len() {
			if err := validateValue(value.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			if err := validateValue(iter.Value()); err != nil {
				return err
			}
		}
	default:
		return nil
	}

	return nil
}

// validateCustomTags applies registered custom tag validators to exported struct fields.
func validateCustomTags(value reflect.Value, path string) error {
	value = dereference(value)
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return nil
	}

	typ := value.Type()

	for i := range value.NumField() {
		fieldType := typ.Field(i)
		if fieldType.PkgPath != "" {
			continue
		}

		fieldValue := value.Field(i)

		fieldPath := fieldType.Name
		if path != "" {
			fieldPath = path + "." + fieldType.Name
		}

		if err := validateFieldTag(fieldType.Tag.Get("validate"), fieldValue, fieldPath); err != nil {
			return err
		}

		// Struct and pointer fields are already covered by the engine, so only
		// collection elements need standard tags applied while walking.
		if err := walkNestedCustomTags(fieldValue, fieldPath, false); err != nil {
			return err
		}
	}

	return nil
}

// walkNestedCustomTags descends into nested structs and collections for custom tag validation.
// standardTags requests engine validation for structs the engine does not reach on its own.
func walkNestedCustomTags(value reflect.Value, path string, standardTags bool) error {
	value = dereference(value)
	if !value.IsValid() {
		return nil
	}

	switch value.Kind() {
	case reflect.Struct:
		if standardTags {
			if err := engine.Struct(value.Interface()); err != nil {
				return err
			}
		}

		return validateCustomTags(value, path)
	case reflect.Slice, reflect.Array:
		for i := range value.Len() {
			itemPath := fmt.Sprintf("%s[%d]", path, i)
			// The engine does not descend into collection elements, so structs
			// found here still need their standard tags validated.
			if err := walkNestedCustomTags(value.Index(i), itemPath, true); err != nil {
				return err
			}
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			if err := walkNestedCustomTags(iter.Value(), path, true); err != nil {
				return err
			}
		}
	default:
		return nil
	}

	return nil
}

// validateFieldTag executes registered custom validators found in a validate tag.
func validateFieldTag(tag string, value reflect.Value, path string) error {
	if tag == "" || tag == "-" {
		return nil
	}

	for part := range strings.SplitSeq(tag, ",") {
		name, param, _ := strings.Cut(strings.TrimSpace(part), "=")

		// The lock is not held while calling fn, otherwise a validator that
		// registers another tag would deadlock and wedge all later validation.
		fn, ok := lookupValidationFunc(name)
		if !ok {
			continue
		}

		fieldValue := dereference(value)
		if !fieldValue.IsValid() {
			continue
		}

		if err := fn(fieldValue.Interface(), param); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}

	return nil
}

// dereference unwraps pointers and interfaces until it reaches a concrete value or nil.
func dereference(value reflect.Value) reflect.Value {
	for value.IsValid() && (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) {
		if value.IsNil() {
			return reflect.Value{}
		}

		value = value.Elem()
	}

	return value
}
