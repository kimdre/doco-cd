package git

import "github.com/go-git/go-git/v5/plumbing"

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
