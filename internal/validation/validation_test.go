package validation

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestTextError(t *testing.T) {
	inner := errors.New("boom")
	err := TextError{Err: inner}

	if got := err.Error(); got != "boom" {
		t.Fatalf("Error() = %q, want %q", got, "boom")
	}

	if !errors.Is(err, inner) {
		t.Fatal("expected errors.Is to match wrapped error")
	}
}

func TestTextErrorNil(t *testing.T) {
	err := TextError{}

	if got := err.Error(); got != "" {
		t.Fatalf("Error() = %q, want empty string", got)
	}

	if err.Unwrap() != nil {
		t.Fatalf("Unwrap() = %v, want nil", err.Unwrap())
	}
}

func TestRegisterValidationFuncRejectsEmptyTag(t *testing.T) {
	err := RegisterValidationFunc("   ", func(_ any, _ string) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "cannot be empty") {
		t.Fatalf("expected empty tag error, got %v", err)
	}
}

func TestRegisterValidationFuncRejectsNilFunc(t *testing.T) {
	tag := "validationtestnilfunc"

	err := RegisterValidationFunc(tag, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "cannot be nil") {
		t.Fatalf("expected nil function error, got %v", err)
	}
}

func TestRegisterValidationFuncRejectsDuplicateTag(t *testing.T) {
	tag := "validationtestduplicate"

	if err := RegisterValidationFunc(tag, func(_ any, _ string) error {
		return nil
	}); err != nil {
		t.Fatalf("first RegisterValidationFunc() error = %v", err)
	}

	err := RegisterValidationFunc(tag, func(_ any, _ string) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected duplicate registration error, got nil")
	}

	if !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("expected duplicate registration error, got %v", err)
	}
}

func TestValidateSupportsNonzeroAlias(t *testing.T) {
	type cfg struct {
		Name string `validate:"nonzero"`
	}

	if err := Validate(cfg{Name: "doco"}); err != nil {
		t.Fatalf("Validate(valid) error = %v", err)
	}

	if err := Validate(cfg{}); err == nil {
		t.Fatal("expected validation error for empty required field")
	}
}

func TestValidateRunsCustomValidationForNestedFields(t *testing.T) {
	tag := "validationtestnested"
	expectedErr := errors.New("invalid code")

	if err := RegisterValidationFunc(tag, func(v any, param string) error {
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("unexpected type %T", v)
		}

		if s != param {
			return expectedErr
		}

		return nil
	}); err != nil {
		t.Fatalf("RegisterValidationFunc() error = %v", err)
	}

	type child struct {
		Code string `validate:"required,validationtestnested=ok"`
	}

	type parent struct {
		Pointer *child
		Items   []child
		Lookup  map[string]child
	}

	valid := parent{
		Pointer: &child{Code: "ok"},
		Items:   []child{{Code: "ok"}},
		Lookup:  map[string]child{"a": {Code: "ok"}},
	}
	if err := Validate(valid); err != nil {
		t.Fatalf("Validate(valid) error = %v", err)
	}

	invalidPointer := parent{
		Pointer: &child{Code: "bad"},
	}

	err := Validate(invalidPointer)
	if err == nil {
		t.Fatal("expected custom validation error for pointer field")
	}

	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected wrapped custom error, got %v", err)
	}

	if !strings.Contains(err.Error(), "Pointer.Code") {
		t.Fatalf("expected pointer field path in error, got %v", err)
	}

	invalidSlice := parent{
		Items: []child{{Code: "bad"}},
	}

	err = Validate(invalidSlice)
	if err == nil {
		t.Fatal("expected custom validation error for slice field")
	}

	if !strings.Contains(err.Error(), "Items[0].Code") {
		t.Fatalf("expected slice field path in error, got %v", err)
	}

	invalidMap := parent{
		Lookup: map[string]child{"a": {Code: "bad"}},
	}

	err = Validate(invalidMap)
	if err == nil {
		t.Fatal("expected custom validation error for map field")
	}

	if !strings.Contains(err.Error(), "Lookup.Code") {
		t.Fatalf("expected map field path in error, got %v", err)
	}
}

func TestValidateSkipsNilPointersForCustomValidation(t *testing.T) {
	tag := "validationtestnilptr"

	if err := RegisterValidationFunc(tag, func(_ any, _ string) error {
		return errors.New("should not be called")
	}); err != nil {
		t.Fatalf("RegisterValidationFunc() error = %v", err)
	}

	type child struct {
		Code string `validate:"validationtestnilptr=ok"`
	}

	type cfg struct {
		Child *child
	}

	if err := Validate(cfg{}); err != nil {
		t.Fatalf("Validate(nil pointer) error = %v", err)
	}
}
