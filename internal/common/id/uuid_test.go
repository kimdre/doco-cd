package id

import (
	"testing"
	"uuid"
)

func TestNew(t *testing.T) {
	t.Parallel()

	got := New()

	_, err := uuid.Parse(got)
	if err != nil {
		t.Fatalf("New() returned invalid UUID %q: %v", got, err)
	}
}
