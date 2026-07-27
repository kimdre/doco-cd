package module

import (
	"errors"
	"testing"
)

func TestGetVersion(t *testing.T) {
	t.Parallel()

	if _, err := GetVersion("example.invalid/not-present"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetVersion() error = %v, want ErrNotFound", err)
	}
}
