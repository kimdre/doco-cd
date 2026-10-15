package validation

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type selfValidatedScalar int

func (v selfValidatedScalar) ValidateValue() error {
	if v < 0 {
		return errors.New("value must not be negative")
	}

	return nil
}

type selfValidatedStruct struct {
	Value selfValidatedScalar
}

func (v selfValidatedStruct) ValidateValue() error {
	if v.Value < -1 {
		return errors.New("invalid struct value")
	}

	return nil
}

func TestValidateRespectsTraversalTags(t *testing.T) {
	for _, test := range []struct {
		name    string
		value   any
		wantErr string
	}{
		{
			name: "skip scalar",
			value: struct {
				Value selfValidatedScalar `validate:"-"`
			}{Value: -1},
		},
		{
			name: "skip struct",
			value: struct {
				Child *selfValidatedStruct `validate:"-"`
			}{Child: &selfValidatedStruct{Value: -2}},
		},
		{
			name: "nostructlevel pointer",
			value: struct {
				Child *selfValidatedStruct `validate:"required,nostructlevel"`
			}{Child: &selfValidatedStruct{Value: -2}},
		},
		{
			name: "nostructlevel interface",
			value: struct {
				Child any `validate:"required,nostructlevel"`
			}{Child: &selfValidatedStruct{Value: -2}},
		},
		{
			name: "structonly skips fields",
			value: struct {
				Child *selfValidatedStruct `validate:"required,structonly"`
			}{Child: &selfValidatedStruct{Value: -1}},
		},
		{
			name: "structonly validates self",
			value: struct {
				Child *selfValidatedStruct `validate:"required,structonly"`
			}{Child: &selfValidatedStruct{Value: -2}},
			wantErr: "Child: invalid struct value",
		},
		{
			name: "structonly optional nil",
			value: struct {
				Child *selfValidatedStruct `validate:"omitempty,structonly"`
			}{},
		},
		{
			name: "nostructlevel zero value",
			value: struct {
				Child selfValidatedStruct `validate:"required,nostructlevel"`
			}{},
			wantErr: "'required'",
		},
		{
			name: "nostructlevel nonzero value",
			value: struct {
				Child selfValidatedStruct `validate:"required,nostructlevel"`
			}{Child: selfValidatedStruct{Value: -2}},
		},
		{
			name: "nostructlevel optional zero value",
			value: struct {
				Child selfValidatedStruct `validate:"omitempty,nostructlevel"`
			}{},
		},
		{
			name: "omitted parent skips opaque presence checks",
			value: struct {
				Parent struct {
					Child selfValidatedStruct `validate:"required,nostructlevel"`
				} `validate:"omitempty"`
			}{},
		},
		{
			name: "non-omitted parent keeps opaque presence checks",
			value: struct {
				Parent struct {
					Child selfValidatedStruct `validate:"required,nostructlevel"`
				}
			}{},
			wantErr: "'required'",
		},
		{
			name: "nostructlevel pointer to zero value",
			value: struct {
				Child *selfValidatedStruct `validate:"required,nostructlevel"`
			}{Child: &selfValidatedStruct{}},
		},
		{
			name: "nostructlevel typed nil interface",
			value: struct {
				Child any `validate:"required,nostructlevel"`
			}{Child: (*selfValidatedStruct)(nil)},
			wantErr: "'required'",
		},
		{
			name: "nostructlevel required nil",
			value: struct {
				Child *selfValidatedStruct `validate:"required,nostructlevel"`
			}{},
			wantErr: "'required'",
		},
		{
			name: "structonly required nil",
			value: struct {
				Child *selfValidatedStruct `validate:"required,structonly"`
			}{},
			wantErr: "'required'",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := Validate(test.value)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("expected error containing %q, got %v", test.wantErr, err)
			}
		})
	}
}

func TestValidateTraversalTagsSkipNestedCustomValidators(t *testing.T) {
	if err := RegisterValidationFunc("validationtesttraversal", func(_ any, _ string) error {
		return errors.New("nested custom validator must not run")
	}); err != nil {
		t.Fatalf("RegisterValidationFunc() error = %v", err)
	}

	type child struct {
		Code string `validate:"validationtesttraversal"`
	}

	for _, test := range []struct {
		name  string
		value any
	}{
		{
			name: "skip",
			value: struct {
				Child *child `validate:"-"`
			}{Child: &child{}},
		},
		{
			name: "nostructlevel",
			value: struct {
				Child *child `validate:"required,nostructlevel"`
			}{Child: &child{}},
		},
		{
			name: "structonly",
			value: struct {
				Child *child `validate:"required,structonly"`
			}{Child: &child{}},
		},
		{
			name: "skip collection",
			value: struct {
				Children []child `validate:"-"`
			}{Children: []child{{}}},
		},
		{
			name: "field custom validator after nostructlevel",
			value: struct {
				Child *child `validate:"nostructlevel,validationtesttraversal"`
			}{Child: &child{}},
		},
		{
			name: "field custom validator after structonly",
			value: struct {
				Child *child `validate:"structonly,validationtesttraversal"`
			}{Child: &child{}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := Validate(test.value); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestValidateOpaqueStructFieldRules(t *testing.T) {
	expectedErr := errors.New("invalid opaque field")

	if err := RegisterValidationFunc("validationtestopaque", func(_ any, _ string) error {
		return expectedErr
	}); err != nil {
		t.Fatalf("RegisterValidationFunc() error = %v", err)
	}

	err := Validate(struct {
		Child *selfValidatedStruct `validate:"required,validationtestopaque,nostructlevel"`
	}{Child: &selfValidatedStruct{}})
	if !errors.Is(err, expectedErr) || !strings.Contains(err.Error(), "Child") {
		t.Fatalf("expected custom field error, got %v", err)
	}

	err = Validate(struct {
		Child *selfValidatedStruct `validate:"required_without=Other,nostructlevel"`
		Other string
	}{Child: &selfValidatedStruct{}})
	if err == nil || !strings.Contains(err.Error(), `unsupported validation "required_without=Other"`) {
		t.Fatalf("expected unsupported opaque field rule error, got %v", err)
	}
}

func TestValidateConcurrentDependency(t *testing.T) {
	type dependency struct {
		state atomic.Uint64
	}

	dep := &dependency{}
	started := make(chan struct{})
	stopped := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)

		close(started)

		for {
			select {
			case <-stopped:
				return
			default:
				dep.state.Add(1)
			}
		}
	}()

	<-started
	t.Cleanup(func() {
		close(stopped)
		<-done
	})

	type holder struct {
		Dependency any `validate:"required,nostructlevel"`
	}

	for _, test := range []struct {
		name     string
		validate func() error
	}{
		{
			name: "non-validator",
			validate: func() error {
				return validateSelf(reflect.ValueOf(dep).Elem(), "")
			},
		},
		{
			name: "opaque dependency",
			validate: func() error {
				return Validate(holder{Dependency: dep})
			},
		},
		{
			name: "optional opaque dependency",
			validate: func() error {
				return Validate(struct {
					Dependency any `validate:"omitempty,nostructlevel"`
				}{Dependency: dep})
			},
		},
		{
			name: "structonly dependency",
			validate: func() error {
				return Validate(struct {
					Dependency any `validate:"required,structonly"`
				}{Dependency: dep})
			},
		},
		{
			name: "skipped dependency",
			validate: func() error {
				return Validate(struct {
					Dependency any `validate:"-"`
				}{Dependency: dep})
			},
		},
		{
			name: "nested dependency",
			validate: func() error {
				return Validate(struct{ Nested *holder }{Nested: &holder{Dependency: dep}})
			},
		},
		{
			name: "slice dependency",
			validate: func() error {
				return Validate(struct{ Items []holder }{Items: []holder{{Dependency: dep}}})
			},
		},
		{
			name: "slice dive dependency",
			validate: func() error {
				return Validate(struct {
					Items []holder `validate:"dive"`
				}{Items: []holder{{Dependency: dep}}})
			},
		},
		{
			name: "map dive dependency",
			validate: func() error {
				return Validate(struct {
					Items map[string]holder `validate:"dive"`
				}{Items: map[string]holder{"live": {Dependency: dep}}})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			for range 1000 {
				if err := test.validate(); err != nil {
					t.Fatalf("validation error = %v", err)
				}
			}
		})
	}
}

func TestValidateSelfValidatedScalars(t *testing.T) {
	t.Parallel()

	type child struct {
		Value selfValidatedScalar
	}

	negative := selfValidatedScalar(-1)
	for _, test := range []struct {
		name  string
		value any
		path  string
	}{
		{name: "root", value: negative},
		{name: "field", value: child{Value: negative}, path: "Value"},
		{name: "pointer", value: struct{ Child *child }{Child: &child{Value: negative}}, path: "Child.Value"},
		{name: "slice", value: struct{ Values []selfValidatedScalar }{Values: []selfValidatedScalar{negative}}, path: "Values[0]"},
		{name: "map", value: struct {
			Values map[string]selfValidatedScalar
		}{Values: map[string]selfValidatedScalar{"bad": negative}}, path: "Values"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := Validate(test.value)
			if err == nil || !strings.Contains(err.Error(), "must not be negative") || !strings.Contains(err.Error(), test.path) {
				t.Fatalf("expected scalar validation error with path %q, got %v", test.path, err)
			}
		})
	}

	for _, value := range []any{selfValidatedScalar(1), child{}, struct{ Child *child }{}, (*selfValidatedScalar)(nil)} {
		if err := Validate(value); err != nil {
			t.Fatalf("valid scalar or nil pointer was rejected: %v", err)
		}
	}
}

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
		Name string `validate:"required"`
	}

	if err := Validate(cfg{Name: "doco"}); err != nil {
		t.Fatalf("Validate(valid) error = %v", err)
	}

	if err := Validate(cfg{}); err == nil {
		t.Fatal("expected validation error for empty required field")
	}
}

func TestValidateValueHandlesSliceBranch(t *testing.T) {
	type child struct {
		Code string `validate:"required"`
	}

	err := validateValue(reflect.ValueOf([]child{{Code: ""}}))
	if err == nil {
		t.Fatal("expected validation error for slice element")
	}
}

func TestValidateValueHandlesArrayBranch(t *testing.T) {
	type child struct {
		Code string `validate:"required"`
	}

	err := validateValue(reflect.ValueOf([1]child{{Code: ""}}))
	if err == nil {
		t.Fatal("expected validation error for array element")
	}
}

func TestValidateValueHandlesMapBranch(t *testing.T) {
	type child struct {
		Code string `validate:"required"`
	}

	err := validateValue(reflect.ValueOf(map[string]child{"a": {Code: ""}}))
	if err == nil {
		t.Fatal("expected validation error for map value")
	}
}

func TestValidateValueHandlesDefaultBranch(t *testing.T) {
	if err := validateValue(reflect.ValueOf(42)); err != nil {
		t.Fatalf("expected nil for default branch, got %v", err)
	}
}

func TestValidateValueHandlesInvalidDereferencedValue(t *testing.T) {
	var n *int

	if err := validateValue(reflect.ValueOf(n)); err != nil {
		t.Fatalf("expected nil for nil pointer after dereference, got %v", err)
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

// Standard tags must also apply to structs held in slice and map fields, which
// the underlying engine does not descend into on its own.
func TestValidateAppliesStandardTagsInsideCollections(t *testing.T) {
	type child struct {
		Code string `validate:"required"`
	}

	type sliceHolder struct {
		Items []child
	}

	type mapHolder struct {
		Lookup map[string]child
	}

	if err := Validate(sliceHolder{Items: []child{{Code: "ok"}}}); err != nil {
		t.Fatalf("Validate(valid slice) error = %v", err)
	}

	if err := Validate(sliceHolder{Items: []child{{Code: ""}}}); err == nil {
		t.Fatal("expected validation error for empty field in slice element")
	}

	if err := Validate(mapHolder{Lookup: map[string]child{"a": {Code: "ok"}}}); err != nil {
		t.Fatalf("Validate(valid map) error = %v", err)
	}

	if err := Validate(mapHolder{Lookup: map[string]child{"a": {Code: ""}}}); err == nil {
		t.Fatal("expected validation error for empty field in map value")
	}
}

// A custom validator that registers another tag must not deadlock, since the
// registry lock must not be held while user callbacks run.
func TestValidateDoesNotDeadlockOnReentrantRegistration(t *testing.T) {
	if err := RegisterValidationFunc("validationtestreentrant", func(_ any, _ string) error {
		_ = RegisterValidationFunc("validationtestreentrantinner", func(_ any, _ string) error {
			return nil
		})

		return nil
	}); err != nil {
		t.Fatalf("RegisterValidationFunc() error = %v", err)
	}

	type cfg struct {
		Code string `validate:"validationtestreentrant=x"`
	}

	done := make(chan error, 1)

	go func() {
		done <- Validate(cfg{Code: "x"})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Validate() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Validate() deadlocked on reentrant registration")
	}
}

// A rejected tag must surface as an error and must not be retained as registered.
func TestRegisterValidationFuncDoesNotRetainFailedRegistration(t *testing.T) {
	fn := func(_ any, _ string) error {
		return nil
	}

	// "dive" is reserved by the underlying engine, so registration must fail.
	first := RegisterValidationFunc("dive", fn)
	if first == nil {
		t.Fatal("expected error when registering a reserved tag")
	}

	second := RegisterValidationFunc("dive", fn)
	if second == nil {
		t.Fatal("expected repeated registration to fail again")
	}

	if strings.Contains(second.Error(), "already registered") {
		t.Fatalf("failed registration was retained, got %v", second)
	}
}
