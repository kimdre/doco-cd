package set

// Set represents a generic set data structure.
type Set[T comparable] map[T]struct{}

// New creates a new set and initializes it with the provided elements.
func New[T comparable](elements ...T) Set[T] {
	s := Set[T]{}
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

// ToSlice converts the set to a slice of its elements.
func (s Set[T]) ToSlice() []T {
	slice := make([]T, 0, len(s))
	for elem := range s {
		slice = append(slice, elem)
	}

	return slice
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

func (s Set[T]) Len() int {
	return len(s)
}
