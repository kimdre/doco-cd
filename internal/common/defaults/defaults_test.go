package defaults

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

type nested struct {
	Enabled bool `default:"true"`
}

type item struct {
	Name string `default:"item"`
}

type sample struct {
	Name       string            `default:"demo"`
	Count      int               `default:"42"`
	Delay      time.Duration     `default:"15s"`
	Items      []string          `default:"[\"one\",\"two\"]"`
	Children   []item            `default:"[{\"Name\":\"\"}]"`
	Labels     map[string]item   `default:"{\"primary\":{}}"`
	NilAllowed map[string]string `default:"{}"`
	Internal   nested
}

type invalid struct {
	Delay time.Duration `default:"not-a-duration"`
}

type nilFieldsSample struct {
	Ptr   *nested
	Iface any
	M     map[string]item
}

type sliceErrorSample struct {
	Items []invalid
}

type mapErrorSample struct {
	Items map[string]invalid
}

type emptyDefaultSample struct {
	Items []string `default:""`
}

type mapOfScalarsSample struct {
	Counts map[string]int
}

type mapOfPointersSample struct {
	Values map[string]*item
}

type ifaceSample struct {
	Iface any
}

type unexportedFieldSample struct {
	Public string `default:"public"`
	hidden string `default:"hidden"` //nolint:unused // exercises the unexported-field skip path
}

type durationEmptyDefaultSample struct {
	Delay time.Duration `default:""`
}

type malformedDefaultSample struct {
	Items []string `default:"[invalid"`
}

type pointerSample struct {
	Value *string `default:"foo"`
}

type arraySample struct {
	Codes [2]string `default:"[\"a\",\"b\"]"`
}

type mapOfSlices struct {
	Groups map[string][]item `default:"{\"g1\":[{}]}"`
}

type mapOfMaps struct {
	Outer map[string]map[string]item `default:"{\"o1\":{\"i1\":{}}}"`
}

func TestSet(t *testing.T) {
	t.Parallel()

	s := sample{}
	if err := Set(&s); err != nil {
		t.Fatalf("set: %v", err)
	}

	if s.Name != "demo" || s.Count != 42 || s.Delay != 15*time.Second {
		t.Fatalf("unexpected defaults: %#v", s)
	}

	if !reflect.DeepEqual(s.Items, []string{"one", "two"}) {
		t.Fatalf("unexpected items: %#v", s.Items)
	}

	if len(s.Children) != 1 || s.Children[0].Name != "item" {
		t.Fatalf("expected nested slice defaults to be applied: %#v", s.Children)
	}

	if got := s.Labels["primary"]; got.Name != "item" {
		t.Fatalf("expected nested map defaults to be applied: %#v", s.Labels)
	}

	if !s.Internal.Enabled {
		t.Fatalf("expected nested defaults to be applied")
	}
}

func TestSet_DoesNotOverwriteExistingValues(t *testing.T) {
	t.Parallel()

	s := sample{
		Name:  "custom",
		Count: 7,
		Delay: 3 * time.Minute,
		Children: []item{{
			Name: "preserve",
		}},
		Labels: map[string]item{
			"primary": {Name: "keep"},
		},
		Internal: nested{
			Enabled: true,
		},
	}

	if err := Set(&s); err != nil {
		t.Fatalf("set: %v", err)
	}

	if s.Name != "custom" || s.Count != 7 || s.Delay != 3*time.Minute || !s.Internal.Enabled {
		t.Fatalf("expected existing values to remain untouched: %#v", s)
	}

	if s.Children[0].Name != "preserve" {
		t.Fatalf("expected nested slice value to remain untouched: %#v", s.Children)
	}

	if s.Labels["primary"].Name != "keep" {
		t.Fatalf("expected nested map value to remain untouched: %#v", s.Labels)
	}
}

// TestSet_PointerField covers both defaulting a nil pointer field and
// preserving an already-set pointer field.
func TestSet_PointerField(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		initial *string
		want    string
	}{
		{name: "nil pointer is defaulted", initial: nil, want: "foo"},
		{name: "existing pointer is preserved", initial: new("bar"), want: "bar"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := pointerSample{Value: tc.initial}
			if err := Set(&s); err != nil {
				t.Fatalf("set: %v", err)
			}

			if s.Value == nil || *s.Value != tc.want {
				t.Fatalf("expected %q, got %#v", tc.want, s.Value)
			}
		})
	}
}

// TestSet_AppliesDefaultsThroughContainers groups scenarios where defaults
// must be applied through one or more layers of indirection: arrays, nested
// maps/slices, pointers stored in a map, and interface values.
func TestSet_AppliesDefaultsThroughContainers(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "array field",
			run: func(t *testing.T) {
				s := arraySample{}
				if err := Set(&s); err != nil {
					t.Fatalf("set: %v", err)
				}

				if !reflect.DeepEqual(s.Codes, [2]string{"a", "b"}) {
					t.Fatalf("unexpected array defaults: %#v", s.Codes)
				}
			},
		},
		{
			name: "map of slices",
			run: func(t *testing.T) {
				s := mapOfSlices{}
				if err := Set(&s); err != nil {
					t.Fatalf("set: %v", err)
				}

				group, ok := s.Groups["g1"]
				if !ok || len(group) != 1 || group[0].Name != "item" {
					t.Fatalf("expected nested slice-in-map defaults to be applied: %#v", s.Groups)
				}
			},
		},
		{
			name: "map of maps",
			run: func(t *testing.T) {
				s := mapOfMaps{}
				if err := Set(&s); err != nil {
					t.Fatalf("set: %v", err)
				}

				inner, ok := s.Outer["o1"]
				if !ok {
					t.Fatalf("expected outer map key to exist: %#v", s.Outer)
				}

				if got := inner["i1"]; got.Name != "item" {
					t.Fatalf("expected nested map-in-map defaults to be applied: %#v", inner)
				}
			},
		},
		{
			name: "pointer stored as a map value",
			run: func(t *testing.T) {
				target := &item{}
				s := mapOfPointersSample{Values: map[string]*item{"a": target}}

				if err := Set(&s); err != nil {
					t.Fatalf("set: %v", err)
				}

				if target.Name != "item" {
					t.Fatalf("expected defaults to be applied through the pointer, got %#v", target)
				}
			},
		},
		{
			name: "interface field holding a pointer",
			run: func(t *testing.T) {
				target := &item{}
				s := ifaceSample{Iface: target}

				if err := Set(&s); err != nil {
					t.Fatalf("set: %v", err)
				}

				if target.Name != "item" {
					t.Fatalf("expected defaults to be applied through the interface value, got %#v", target)
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t)
		})
	}
}

// TestSet_LeavesUnannotatedValuesUntouched groups scenarios where Set must
// not panic or error, and must leave values without applicable defaults
// exactly as they were.
func TestSet_LeavesUnannotatedValuesUntouched(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "nil pointer, interface, and map fields",
			run: func(t *testing.T) {
				s := nilFieldsSample{}
				if err := Set(&s); err != nil {
					t.Fatalf("set: %v", err)
				}

				if s.Ptr != nil {
					t.Fatalf("expected nil pointer field to remain nil, got %#v", s.Ptr)
				}

				if s.Iface != nil {
					t.Fatalf("expected nil interface field to remain nil, got %#v", s.Iface)
				}

				if s.M != nil {
					t.Fatalf("expected nil map field to remain nil, got %#v", s.M)
				}
			},
		},
		{
			name: "map of scalars",
			run: func(t *testing.T) {
				s := mapOfScalarsSample{Counts: map[string]int{"a": 1}}

				if err := Set(&s); err != nil {
					t.Fatalf("set: %v", err)
				}

				if s.Counts["a"] != 1 {
					t.Fatalf("expected scalar map value to remain untouched: %#v", s.Counts)
				}
			},
		},
		{
			name: "map with a nil pointer entry",
			run: func(t *testing.T) {
				s := mapOfPointersSample{Values: map[string]*item{"a": nil}}

				if err := Set(&s); err != nil {
					t.Fatalf("set: %v", err)
				}

				if s.Values["a"] != nil {
					t.Fatalf("expected nil map entry to remain nil, got %#v", s.Values["a"])
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t)
		})
	}
}

// TestSet_SkipsUnexportedFields verifies that unexported fields are ignored
// even when tagged with a default, while sibling exported fields are still
// defaulted.
func TestSet_SkipsUnexportedFields(t *testing.T) {
	t.Parallel()

	s := unexportedFieldSample{}
	if err := Set(&s); err != nil {
		t.Fatalf("set: %v", err)
	}

	if s.Public != "public" {
		t.Fatalf("expected exported field to be defaulted, got %q", s.Public)
	}

	if s.hidden != "" {
		t.Fatalf("expected unexported field to remain untouched, got %q", s.hidden)
	}
}

// TestSet_EmptyDefaultTagResolvesToZeroValue groups cases where an empty
// `default:""` tag must resolve to the field's zero value instead of being
// parsed (e.g. as YAML or a duration string).
func TestSet_EmptyDefaultTagResolvesToZeroValue(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "non-scalar field (slice)",
			run: func(t *testing.T) {
				s := emptyDefaultSample{}
				if err := Set(&s); err != nil {
					t.Fatalf("set: %v", err)
				}

				if s.Items != nil {
					t.Fatalf("expected zero value (nil slice), got %#v", s.Items)
				}
			},
		},
		{
			name: "time.Duration field",
			run: func(t *testing.T) {
				s := durationEmptyDefaultSample{}
				if err := Set(&s); err != nil {
					t.Fatalf("set: %v", err)
				}

				if s.Delay != 0 {
					t.Fatalf("expected zero duration, got %v", s.Delay)
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t)
		})
	}
}

func TestSet_RejectsInvalidTarget(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		value any
	}{
		{name: "nil", value: nil},
		{name: "non-pointer", value: sample{}},
		{name: "pointer to non-struct", value: new(int)},
		{name: "nil pointer", value: (*sample)(nil)},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if err := Set(tc.value); !errors.Is(err, ErrInvalidTarget) {
				t.Fatalf("expected ErrInvalidTarget, got %v", err)
			}
		})
	}
}

// TestSet_ReturnsErrors groups scenarios where Set must surface an error
// instead of panicking: an invalid tag value directly on a field, and the
// same failure occurring one level deep inside a slice or map.
func TestSet_ReturnsErrors(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		target  any
		wantErr string // exact match when non-empty; otherwise any non-nil error is accepted
	}{
		{
			name:    "invalid duration tag on a struct field",
			target:  &invalid{},
			wantErr: `Delay: time: invalid duration "not-a-duration"`,
		},
		{
			name:   "invalid tag on a slice element",
			target: &sliceErrorSample{Items: []invalid{{}}},
		},
		{
			name:   "invalid tag on a map entry",
			target: &mapErrorSample{Items: map[string]invalid{"a": {}}},
		},
		{
			name:   "malformed yaml in a default tag",
			target: &malformedDefaultSample{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := Set(tc.target)
			if err == nil {
				t.Fatal("expected error, got nil")
			}

			if tc.wantErr != "" && err.Error() != tc.wantErr {
				t.Fatalf("unexpected error: got %q, want %q", err.Error(), tc.wantErr)
			}
		})
	}
}
