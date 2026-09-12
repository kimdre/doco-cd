package git

import (
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TreeFS is a read-only fs.FS view over a single commit's tree. It lets
// callers inspect a reference's contents (directory listings and file
// contents) without checking it out into the repository's working tree,
// so read-only consumers such as auto-discovery no longer need to mutate
// (and race on) the shared working tree to look at a different reference.
type TreeFS struct {
	tree   *object.Tree
	commit plumbing.Hash
}

var (
	_ fs.FS         = (*TreeFS)(nil)
	_ fs.ReadDirFS  = (*TreeFS)(nil)
	_ fs.ReadFileFS = (*TreeFS)(nil)
)

// NewTreeFS resolves ref in repo to a commit using the same resolution rules
// CheckoutRepository uses, and returns a TreeFS backed by that commit's tree.
func NewTreeFS(repo *git.Repository, ref string) (*TreeFS, error) {
	hash, err := ResolveReferenceCommit(repo, ref)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve reference %s: %w", ref, err)
	}

	return NewTreeFSAtCommit(repo, hash)
}

// NewTreeFSAtCommit returns a TreeFS backed by the tree of the given commit.
func NewTreeFSAtCommit(repo *git.Repository, commit plumbing.Hash) (*TreeFS, error) {
	commitObj, err := repo.CommitObject(commit)
	if err != nil {
		return nil, fmt.Errorf("failed to get commit object %s: %w", commit, err)
	}

	tree, err := commitObj.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get tree for commit %s: %w", commit, err)
	}

	return &TreeFS{tree: tree, commit: commit}, nil
}

// Commit returns the commit hash this TreeFS is rooted at.
func (t *TreeFS) Commit() plumbing.Hash {
	return t.commit
}

// subtree resolves name (an fs.FS-style slash path, "." for the root) to the
// *object.Tree it identifies.
func (t *TreeFS) subtree(name string) (*object.Tree, error) {
	if name == "." {
		return t.tree, nil
	}

	return t.tree.Tree(name)
}

// Open implements fs.FS. It is primarily used by fs.Stat/fs.WalkDir to stat
// the root; ReadDir and ReadFile are used for everything else.
func (t *TreeFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}

	if _, err := t.subtree(name); err == nil {
		return &treeDirHandle{info: treeDirInfo(name)}, nil
	}

	f, err := t.tree.File(name)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	r, err := f.Reader()
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}

	return &treeFileHandle{
		info: treeFileInfo{name: path.Base(name), size: f.Size, mode: fileEntryMode(f.Mode)},
		r:    r,
	}, nil
}

// ReadDir implements fs.ReadDirFS.
func (t *TreeFS) ReadDir(name string) ([]fs.DirEntry, error) {
	sub, err := t.subtree(name)
	if err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}

	entries := make([]fs.DirEntry, 0, len(sub.Entries))

	for _, e := range sub.Entries {
		entries = append(entries, treeDirEntry{entry: e})
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	return entries, nil
}

// ReadFile implements fs.ReadFileFS.
func (t *TreeFS) ReadFile(name string) ([]byte, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readfile", Path: name, Err: fs.ErrInvalid}
	}

	f, err := t.tree.File(name)
	if err != nil {
		return nil, &fs.PathError{Op: "readfile", Path: name, Err: fs.ErrNotExist}
	}

	contents, err := f.Contents()
	if err != nil {
		return nil, &fs.PathError{Op: "readfile", Path: name, Err: err}
	}

	return []byte(contents), nil
}

func fileEntryMode(m filemode.FileMode) fs.FileMode {
	if m == filemode.Executable {
		return 0o555
	}

	return 0o444
}

// treeFileInfo implements fs.FileInfo for both files and directories.
type treeFileInfo struct {
	name string
	size int64
	mode fs.FileMode
	dir  bool
}

func treeDirInfo(name string) treeFileInfo {
	base := path.Base(name)
	if name == "." {
		base = "."
	}

	return treeFileInfo{name: base, mode: fs.ModeDir | 0o555, dir: true}
}

func (fi treeFileInfo) Name() string       { return fi.name }
func (fi treeFileInfo) Size() int64        { return fi.size }
func (fi treeFileInfo) Mode() fs.FileMode  { return fi.mode }
func (fi treeFileInfo) ModTime() time.Time { return time.Time{} }
func (fi treeFileInfo) IsDir() bool        { return fi.dir }
func (fi treeFileInfo) Sys() any           { return nil }

// treeDirHandle implements fs.File for a directory Open() call. Directory
// listing goes through TreeFS.ReadDir, so Read is not expected to be called.
type treeDirHandle struct {
	info treeFileInfo
}

func (d *treeDirHandle) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *treeDirHandle) Read([]byte) (int, error)   { return 0, io.EOF }
func (d *treeDirHandle) Close() error                { return nil }

// treeFileHandle implements fs.File for a regular file.
type treeFileHandle struct {
	info treeFileInfo
	r    io.ReadCloser
}

func (f *treeFileHandle) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *treeFileHandle) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *treeFileHandle) Close() error                { return f.r.Close() }

// treeDirEntry implements fs.DirEntry for a single object.TreeEntry.
type treeDirEntry struct {
	entry object.TreeEntry
}

func (e treeDirEntry) Name() string { return e.entry.Name }
func (e treeDirEntry) IsDir() bool  { return e.entry.Mode == filemode.Dir }

func (e treeDirEntry) Type() fs.FileMode {
	if e.IsDir() {
		return fs.ModeDir
	}

	if e.entry.Mode == filemode.Symlink {
		return fs.ModeSymlink
	}

	return 0
}

func (e treeDirEntry) Info() (fs.FileInfo, error) {
	if e.IsDir() {
		return treeFileInfo{name: e.entry.Name, mode: fs.ModeDir | 0o555, dir: true}, nil
	}

	return treeFileInfo{name: e.entry.Name, mode: fileEntryMode(e.entry.Mode)}, nil
}
