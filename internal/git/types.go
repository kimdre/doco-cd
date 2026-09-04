package git

import (
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

const (
	DefaultShortSHALength = 7 // Default length for shortened commit SHAs
	RemoteName            = "origin"
	TagPrefix             = "refs/tags/"
	BranchPrefix          = "refs/heads/"
	MainBranch            = "refs/heads/main"
	SwarmModeBranch       = "refs/heads/swarm-mode"
	refSpecAllBranches    = "+refs/heads/*:refs/remotes/origin/*"
	refSpecSingleBranch   = "+refs/heads/%s:refs/remotes/origin/%s"
	refSpecAllTags        = "+refs/tags/*:refs/tags/*"
	refSpecSingleTag      = "+refs/tags/%s:refs/tags/%s"
)

type RefSet struct {
	LocalRef   plumbing.ReferenceName
	RemoteRef  plumbing.ReferenceName
	RemoteHash plumbing.Hash
}

// SyncState describes the work needed to bring a repository to its requested ref.
type SyncState string

const (
	SyncStateCloned  SyncState = "cloned"
	SyncStateUpdated SyncState = "updated"
	SyncStateCurrent SyncState = "current"
)

// SyncResult contains the synchronized repository and the work performed.
type SyncResult struct {
	Repository *git.Repository
	State      SyncState
}
