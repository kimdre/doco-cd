package git

import "testing"

func TestValidateEntryName(t *testing.T) {
	t.Parallel()

	valid := []string{"README.md", "sub-dir", ".gitmodules", "a.b.c", "..hidden"}
	for _, name := range valid {
		if err := validateEntryName(name); err != nil {
			t.Errorf("validateEntryName(%q) error = %v, want nil", name, err)
		}
	}

	invalid := []string{"", ".", "..", "a/b", "a\\b", "../escape"}
	for _, name := range invalid {
		if err := validateEntryName(name); err == nil {
			t.Errorf("validateEntryName(%q) error = nil, want an error", name)
		}
	}
}
