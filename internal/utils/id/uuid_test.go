package id

import (
	"testing"

	"github.com/google/uuid"
)

func TestGenID(t *testing.T) {
	t.Parallel()

	got := GenID()

	parsed, err := uuid.Parse(got)
	if err != nil {
		t.Fatalf("GenID() returned invalid UUID %q: %v", got, err)
	}

	if parsed.Version() != uuid.Version(7) {
		t.Fatalf("GenID() version = %d, want %d", parsed.Version(), 7)
	}
}
