// Package store maps mutable source references to immutable revisions and materialized artifact directories.
// GitStore and OCIStore keep source-specific fetching, verification, and export details behind the Store interface.
package store

import (
	"context"
	"errors"
)

// ErrRevisionNotFound is returned by Publish when the requested Revision is
// not reachable in the store's local state and cannot be materialized
// without an unbounded fetch. Callers should Resolve the owning reference
// first, which fetches exactly what is needed to reach it.
var ErrRevisionNotFound = errors.New("source store: revision not found")

// Revision is an immutable identifier for one version of a source: a Git
// commit SHA, or an OCI manifest digest. Unlike a reference (a branch, tag,
// or "latest"), resolving the same Revision twice always yields the same content.
type Revision string

// String implements fmt.Stringer.
func (r Revision) String() string { return string(r) }

// Artifact is a materialized, read-only directory for one Revision. Once
// Publish returns an Artifact, its Path is never written to again by the
// store that produced it - readers may use it without holding any lock.
type Artifact struct {
	// Revision is the immutable revision this artifact was published for.
	Revision Revision
	// Path is the absolute directory holding the revision's deployable
	// contents.
	Path string
}

// Store maps a source's mutable references to immutable revisions,
// and immutable revisions to materialized artifact directories.
//
// Implementations must make Publish safe for concurrent callers publishing
// the same Revision (idempotent, at-most-once materialization) and
// different Revisions (no shared mutable state between them) at the same
// time. Resolve and Lookup take no lock; they either contact the remote or
// inspect already-immutable local state.
type Store interface {
	// Resolve maps ref (a branch, tag, "latest", or already a Revision) to
	// the Revision it currently points to. It may contact the remote (a Git
	// fetch, a registry HEAD/manifest request) but never modifies a
	// previously published Artifact.
	Resolve(ctx context.Context, ref string) (Revision, error)

	// Publish materializes revision into a read-only Artifact directory and
	// returns it. If an Artifact for revision has already been published,
	// it is returned as-is without redoing the work. Publish is safe to
	// call concurrently for the same or different revisions.
	Publish(ctx context.Context, revision Revision) (Artifact, error)

	// Lookup returns the Artifact for revision if it has already been
	// published, without publishing it. The second return value is false
	// if no artifact exists yet.
	Lookup(revision Revision) (Artifact, bool, error)

	// List returns every Artifact currently published by this store, e.g.
	// for garbage collection or migration.
	List() ([]Artifact, error)
}
