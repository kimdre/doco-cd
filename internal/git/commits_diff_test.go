package git

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestDiffFileFromChangeEntryIgnoresNonFiles(t *testing.T) {
	t.Parallel()

	entry := object.ChangeEntry{
		Name: "dependency",
		TreeEntry: object.TreeEntry{
			Mode: filemode.Submodule,
		},
	}

	if got := diffFileFromChangeEntry(entry); got != nil {
		t.Fatalf("diffFileFromChangeEntry() = %#v, want nil for submodule", got)
	}
}
