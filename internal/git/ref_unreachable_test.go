package git

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5/memfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/memory"
)

func TestIsRefUnreachableError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			name: "object not found",
			err:  plumbing.ErrObjectNotFound,
			want: true,
		},
		{
			name: "wrapped object not found",
			err:  fmt.Errorf("wrapped: %w", plumbing.ErrObjectNotFound),
			want: true,
		},
		{
			name: "reference not found",
			err:  plumbing.ErrReferenceNotFound,
			want: true,
		},
		{
			name: "invalid reference",
			err:  ErrInvalidReference,
			want: true,
		},
		{
			name: "message match",
			err:  errors.New("failed to resolve base commit: object not found"),
			want: true,
		},
		{
			name: "other error",
			err:  errors.New("permission denied"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRefUnreachableError(tt.err); got != tt.want {
				t.Fatalf("IsRefUnreachableError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetChangedFilesBetweenCommits_MissingOldCommit(t *testing.T) {
	repo, err := gogit.Init(memory.NewStorage(), memfs.New())
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	hashes := commitN(t, wt, 2)

	missingOld := plumbing.NewHash(strings.Repeat("a", 40))

	_, err = GetChangedFilesBetweenCommits(repo, missingOld, hashes[len(hashes)-1])
	if err == nil {
		t.Fatal("expected error for missing old commit")
	}

	if !IsRefUnreachableError(err) {
		t.Fatalf("expected IsRefUnreachableError=true, got false, err=%v", err)
	}
}
