package stages

import (
	"errors"
	"slices"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSyncWindowBlockedError(t *testing.T) {
	t.Parallel()

	nextOpen := time.Date(2026, time.March, 10, 11, 0, 0, 0, time.UTC)
	err := error(&SyncWindowBlockedError{Stacks: []string{"api", "web"}, Windows: []string{"freeze"}, NextOpen: nextOpen})

	if !errors.Is(err, ErrSyncWindowBlocked) || !errors.Is(err, ErrSkipDeployment) {
		t.Fatalf("%v does not wrap ErrSyncWindowBlocked and ErrSkipDeployment", err)
	}

	if errors.Is(err, ErrWebhookFilterMismatch) {
		t.Fatal("a sync window error must not match ErrWebhookFilterMismatch")
	}

	want := "deployment of api, web deferred by sync window freeze until 2026-03-10T11:00:00Z"
	if err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}

	err = &SyncWindowBlockedError{Stacks: []string{"web"}, Windows: []string{"freeze"}}
	if want := "deployment of web deferred by sync window freeze"; err.Error() != want {
		t.Fatalf("Error() without NextOpen = %q, want %q", err.Error(), want)
	}
}

func TestSyncWindowCommitStatusDescription(t *testing.T) {
	t.Parallel()

	if got := SyncWindowCommitStatusDescription(time.Time{}); got != "Deferred by sync window" {
		t.Fatalf("description without next open = %q", got)
	}

	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("LoadLocation() failed: %v", err)
	}

	got := SyncWindowCommitStatusDescription(time.Date(2026, time.July, 1, 18, 30, 0, 0, berlin))
	if want := "Deferred by sync window until 2026-07-01T18:30:00+02:00"; got != want {
		t.Fatalf("description = %q, want %q", got, want)
	}

	// Commit status descriptions are limited to 140 characters by GitHub.
	if utf8.RuneCountInString(got) > 140 {
		t.Fatalf("description %q is longer than 140 characters", got)
	}
}

func TestMergeSyncWindowBlocked(t *testing.T) {
	t.Parallel()

	if got := MergeSyncWindowBlocked(nil); got != nil {
		t.Fatalf("MergeSyncWindowBlocked(nil) = %v, want nil", got)
	}

	early := time.Date(2026, time.March, 10, 11, 0, 0, 0, time.UTC)

	got := MergeSyncWindowBlocked([]*SyncWindowBlockedError{
		{Stacks: []string{"web"}, Windows: []string{"freeze"}, NextOpen: early.Add(time.Hour)},
		nil,
		{Stacks: []string{"api"}, Windows: []string{"weekend", "freeze"}},
		{Stacks: []string{"web"}, Windows: []string{"freeze"}, NextOpen: early},
	})

	if !slices.Equal(got.Stacks, []string{"api", "web"}) {
		t.Fatalf("Stacks = %v, want [api web]", got.Stacks)
	}

	if !slices.Equal(got.Windows, []string{"freeze", "weekend"}) {
		t.Fatalf("Windows = %v, want [freeze weekend]", got.Windows)
	}

	if !got.NextOpen.Equal(early) {
		t.Fatalf("NextOpen = %v, want the earliest known %v", got.NextOpen, early)
	}
}
