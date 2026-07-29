package set

import (
	"reflect"
	"testing"
)

func TestSet(t *testing.T) {
	t.Parallel()

	s := New[string]("banana")

	// Test Add and Contains
	s.Add("apple")

	if !s.Contains("apple") || !s.Contains("banana") {
		t.Errorf("expected set to contain 'apple' and 'banana'")
	}

	// Test Remove
	s.Remove("apple")

	if s.Contains("apple") {
		t.Errorf("expected set to not contain 'apple' after removal")
	}

	// Test ToSlice
	s.Add("cherry")
	slice := s.ToSlice()

	expectedElements := map[string]bool{"banana": true, "cherry": true}
	if len(slice) != len(expectedElements) {
		t.Errorf("expected slice length %d, got %d", len(expectedElements), len(slice))
	}

	for _, elem := range slice {
		if !expectedElements[elem] {
			t.Errorf("unexpected element '%s' in slice", elem)
		}
	}

	// Test Contains
	if !s.Contains("banana") || !s.Contains("cherry") {
		t.Errorf("expected set to contain 'banana' and 'cherry'")
	}
}

func TestSet_Difference(t *testing.T) {
	tests := []struct {
		name  string
		s     Set[string]
		other Set[string]
		want  Set[string]
	}{
		{
			name: "other include all in s",
			s: New(
				"apple",
			),
			other: New(
				"apple",
				"banana",
				"cherry",
			),
			want: New[string](),
		},
		{
			name: "s include all in other",
			s: New(
				"apple",
				"banana",
				"cherry",
			),
			other: New(
				"apple",
			),
			want: New(
				"banana",
				"cherry",
			),
		},
		{
			name: "s same with other",
			s: New(
				"apple",
			),
			other: New(
				"apple",
			),
			want: New[string](),
		},
		{
			name:  "s same with other and empty",
			s:     New[string](),
			other: New[string](),
			want:  New[string](),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.s.Difference(tt.other)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Difference() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSet_Union(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sets []Set[string]
		want Set[string]
	}{
		{
			name: "merges unique elements",
			sets: []Set[string]{
				New("apple", "banana"),
				New("banana", "cherry"),
			},
			want: New("apple", "banana", "cherry"),
		},
		{
			name: "handles no sets",
			sets: nil,
			want: New[string](),
		},
		{
			name: "handles empty sets",
			sets: []Set[string]{
				New[string](),
				New[string](),
			},
			want: New[string](),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Union(tt.sets...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Union() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSet_Len(t *testing.T) {
	t.Parallel()

	s := New("apple", "banana")
	if got := s.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2", got)
	}

	s.Remove("apple")

	if got := s.Len(); got != 1 {
		t.Fatalf("Len() after removal = %d, want 1", got)
	}

	if got := New[string]().Len(); got != 0 {
		t.Fatalf("Len() on empty set = %d, want 0", got)
	}
}
