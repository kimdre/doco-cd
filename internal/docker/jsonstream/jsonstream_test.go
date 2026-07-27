package jsonstream

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestErrorReader(t *testing.T) {
	t.Parallel()

	t.Run("returns nil for clean stream", func(t *testing.T) {
		t.Parallel()

		in := strings.NewReader(`{"status":"Pulling from library/alpine"}
{"status":"Digest: sha256:abc"}
`)

		if err := ErrorReader(context.Background(), in); err != nil {
			t.Fatalf("ErrorReader() = %v, want nil", err)
		}
	})

	t.Run("returns json error message", func(t *testing.T) {
		t.Parallel()

		in := strings.NewReader(`{"errorDetail":{"message":"boom"}}`)

		if err := ErrorReader(context.Background(), in); err == nil || err.Error() != "boom" {
			t.Fatalf("ErrorReader() = %v, want boom", err)
		}
	})

	t.Run("detects access denied after repeated no such image messages", func(t *testing.T) {
		t.Parallel()

		in := strings.NewReader(`{"status":"No such image: ghcr.io/example/app:latest"}
{"status":"No such image: ghcr.io/example/app:latest"}
{"status":"No such image: ghcr.io/example/app:latest"}
{"status":"No such image: ghcr.io/example/app:latest"}
`)

		err := ErrorReader(context.Background(), in)
		if !errors.Is(err, ErrImagePullAccessDenied) {
			t.Fatalf("ErrorReader() = %v, want ErrImagePullAccessDenied", err)
		}
	})

	t.Run("honors canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := ErrorReader(ctx, strings.NewReader(`{"status":"ignored"}`)); !errors.Is(err, context.Canceled) {
			t.Fatalf("ErrorReader() = %v, want context.Canceled", err)
		}
	})

	t.Run("returns decode error for malformed json", func(t *testing.T) {
		t.Parallel()

		if err := ErrorReader(context.Background(), strings.NewReader(`{"status":`)); err == nil {
			t.Fatal("ErrorReader() = nil, want error")
		}
	})
}
