package set

import "testing"

func TestSet(t *testing.T) {
	s := New[string]()

	// Test Add and Contains
	s.Add("apple")
	if !s.Contains("apple") {
		t.Errorf("expected set to contain 'apple'")
	}

	// Test Remove
	s.Remove("apple")
	if s.Contains("apple") {
		t.Errorf("expected set to not contain 'apple' after removal")
	}

	// Test ToSlice
	s.Add("banana")
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
