package set

import (
	"cmp"
	"slices"
)

// Set represents a generic set data structure.
type Set[T comparable] map[T]struct{}

// New creates a new set and initializes it with the provided elements.
func New[T comparable](elements ...T) Set[T] {
	s := make(Set[T], len(elements))
	for _, elem := range elements {
		s.Add(elem)
	}

	return s
}

// Add inserts the specified elements into the set.
func (s Set[T]) Add(elements ...T) {
	for _, element := range elements {
		s[element] = struct{}{}
	}
}

// Remove deletes the specified element from the set.
func (s Set[T]) Remove(element T) {
	delete(s, element)
}

// Contains checks if the set contains the specified element.
func (s Set[T]) Contains(element T) bool {
	_, exists := s[element]
	return exists
}

// ToSlice converts the set to a slice of its elements. The order of elements is not guaranteed;
// use SortedSlice if a stable, ordered result is required.
func (s Set[T]) ToSlice() []T {
	slice := make([]T, 0, len(s))
	for elem := range s {
		slice = append(slice, elem)
	}

	return slice
}

// Difference returns a new set containing the elements in s that are not in other.
func (s Set[T]) Difference(other Set[T]) Set[T] {
	difference := New[T]()

	for elem := range s {
		if !other.Contains(elem) {
			difference.Add(elem)
		}
	}

	return difference
}

// Intersects reports whether s and other share at least one common element.
func (s Set[T]) Intersects(other Set[T]) bool {
	smaller, larger := s, other
	if len(other) < len(s) {
		smaller, larger = other, s
	}

	for elem := range smaller {
		if larger.Contains(elem) {
			return true
		}
	}

	return false
}

// Union merges the given sets into a new set containing all unique elements from the input sets.
func Union[T comparable](sets ...Set[T]) Set[T] {
	result := Set[T]{}

	for _, s := range sets {
		for elem := range s {
			result.Add(elem)
		}
	}

	return result
}

// Len returns the number of elements in the set.
func (s Set[T]) Len() int {
	return len(s)
}

// IsEmpty reports whether the set has no elements.
func (s Set[T]) IsEmpty() bool {
	return len(s) == 0
}

// SortedSlice converts the set to a slice sorted in ascending order.
// It is a standalone function rather than a method because sorting requires
// T to satisfy cmp.Ordered, a stricter constraint than Set's own comparable.
func SortedSlice[T cmp.Ordered](s Set[T]) []T {
	slice := s.ToSlice()
	slices.Sort(slice)

	return slice
}
