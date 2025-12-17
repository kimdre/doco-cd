package slice

import "github.com/kimdre/doco-cd/internal/utils/set"

// Unique returns a slice of unique elements from the input slice.
func Unique[T comparable](elements []T) []T {
	uniqueSet := set.New[T](elements...)
	return uniqueSet.ToSlice()
}
