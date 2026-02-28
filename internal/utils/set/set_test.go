package set

import "testing"

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
