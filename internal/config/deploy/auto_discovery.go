package deploy

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/common/types/clone"
	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/filesystem"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
)

// AutoDiscoveryConfig holds auto-discovery settings for a deployment.
type AutoDiscoveryConfig struct {
	ScanDepth     int  `yaml:"depth" json:"depth" default:"0"`                       // ScanDepth is the maximum depth of subdirectories to scan for docker-compose files
	Enabled       bool `yaml:"enabled" json:"enabled" default:"false"`               // Enabled enables autodiscovery of services to deploy in the working directory
	Delete        bool `yaml:"delete" json:"delete" default:"false"`                 // Delete removes obsolete auto-discovered deployments that are no longer present in the repository
	RemoveVolumes bool `yaml:"remove_volumes" json:"remove_volumes" default:"false"` // RemoveVolumes removes the volumes of an auto-discovered deployment when it is deleted
	RemoveImages  bool `yaml:"remove_images" json:"remove_images" default:"true"`    // RemoveImages removes the images of an auto-discovered deployment when it is deleted
}

// discoveryCache is an LRU cache for auto-discovery results.
type discoveryCache struct {
	mu      sync.Mutex
	entries map[discoveryCacheKey]*list.Element
	recent  list.List
	bytes   int
}

// autoDiscoveryCache is a global cache for auto-discovery results,
// keyed by repository, tree, settings, and remaining depth.
var autoDiscoveryCache = discoveryCache{
	entries: make(map[discoveryCacheKey]*list.Element),
}

// Maximum number of entries in the auto-discovery proof cache.
// Each entry is a Git subtree hash, repository hash, and settings hash.
const maxAutoDiscoveryProofEntries = 4096

// discoveryProofKey is a unique key for a Git subtree proof,
// consisting of the tree hash, repository hash, and settings hash.
type discoveryProofKey struct {
	tree       plumbing.Hash
	repository [sha256.Size]byte
	settings   [sha256.Size]byte
}

// discoveryProofCache is an LRU cache for Git subtree proofs,
// allowing reuse of verified subtrees across different artifacts.
type discoveryProofCache struct {
	mu      sync.Mutex
	entries map[discoveryProofKey]*list.Element
	recent  list.List
}

// autoDiscoveryProof is a global cache for Git subtree proofs,
// allowing reuse of verified subtrees across different artifacts.
var autoDiscoveryProof = discoveryProofCache{
	entries: make(map[discoveryProofKey]*list.Element),
}

// get checks if a proof for the given key exists in the cache.
func (c *discoveryProofCache) get(key discoveryProofKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	element, ok := c.entries[key]
	if !ok {
		return false
	}

	c.recent.MoveToBack(element)

	return true
}

// put adds a proof for the given key to the cache, evicting the oldest entry if necessary.
func (c *discoveryProofCache) put(key discoveryProofKey) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.entries == nil {
		c.entries = make(map[discoveryProofKey]*list.Element)
	}

	if element, ok := c.entries[key]; ok {
		c.recent.MoveToBack(element)
		return
	}

	if len(c.entries) >= maxAutoDiscoveryProofEntries {
		oldest := c.recent.Front()
		delete(c.entries, oldest.Value.(discoveryProofKey))
		c.recent.Remove(oldest)
	}

	c.entries[key] = c.recent.PushBack(key)
}

// Maximum number of entries in the auto-discovery cache.
// Each entry is a repository, tree, settings, and remaining depth.
const (
	maxAutoDiscoveryCacheEntries    = 4096
	maxAutoDiscoveryCacheBytes      = 8 << 20
	maxAutoDiscoveryCacheEntryBytes = 256 << 10
)

type discoveryCacheKey struct {
	repository, tree, settings string
	remainingDepth             int
}

type discoveryCacheEntry struct {
	key     discoveryCacheKey
	matches []discoveryMatch
	size    int
}

// Paths in the cache are relative to the cached subtree, not to a revision's artifact.
// Overrides contain parsed user fields only; the base Config (including Internal) is
// cloned and applied afresh for every discovery.
type discoveryMatch struct {
	dir          string
	override     *Config
	overrideSize int
}

// get retrieves the matches for the given key from the cache, returning false if not found.
func (c *discoveryCache) get(key discoveryCacheKey) ([]discoveryMatch, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	element, ok := c.entries[key]
	if !ok {
		return nil, false
	}

	c.recent.MoveToBack(element)

	return element.Value.(*discoveryCacheEntry).matches, true
}

// put adds the matches for the given key to the cache, evicting the oldest entries if necessary.
func (c *discoveryCache) put(key discoveryCacheKey, matches []discoveryMatch) {
	size := 128 + len(key.repository) + len(key.tree) + len(key.settings)
	for _, match := range matches {
		size += 96 + len(match.dir) + match.overrideSize
		if size > maxAutoDiscoveryCacheEntryBytes {
			return
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.entries == nil {
		c.entries = make(map[discoveryCacheKey]*list.Element)
	}

	if element, exists := c.entries[key]; exists {
		c.recent.MoveToBack(element)
		return
	}

	// Evict oldest entries until the cache is within limits.
	for len(c.entries) >= maxAutoDiscoveryCacheEntries || c.bytes+size > maxAutoDiscoveryCacheBytes {
		oldest := c.recent.Front()
		entry := oldest.Value.(*discoveryCacheEntry)
		c.bytes -= entry.size
		delete(c.entries, entry.key)
		c.recent.Remove(oldest)
	}

	entry := &discoveryCacheEntry{key: key, matches: matches, size: size}
	c.entries[key] = c.recent.PushBack(entry)
	c.bytes += size
}

func (c *AutoDiscoveryConfig) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var enabled bool
		if err := node.Decode(&enabled); err != nil {
			return errors.New("invalid auto_discovery value: expected bool or object")
		}

		c.Enabled = enabled

		return nil
	case yaml.MappingNode:
		type plain AutoDiscoveryConfig

		decoded := plain(*c)
		if err := node.Decode(&decoded); err != nil {
			return err
		}

		*c = AutoDiscoveryConfig(decoded)

		return nil
	default:
		return errors.New("invalid auto_discovery value: expected bool or object")
	}
}

func (c *AutoDiscoveryConfig) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("true")) || bytes.Equal(bytes.TrimSpace(data), []byte("false")) {
		var enabled bool
		if err := json.Unmarshal(data, &enabled); err != nil {
			return errors.New("invalid auto_discovery value: expected bool or object")
		}

		c.Enabled = enabled

		return nil
	}

	type plain AutoDiscoveryConfig

	decoded := plain(*c)
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	*c = AutoDiscoveryConfig(decoded)

	return nil
}

// expandInlineAutoDiscoverConfigs replaces enabled inline auto-discovery entries with deployments under repoRoot.
// labelRoot is a revision-stable directory naming the repository; repoRoot itself is usually a per-revision
// artifact directory. revisionKey overrides the repository HEAD when repoRoot is not a Git checkout.
func expandInlineAutoDiscoverConfigs(repoRoot, labelRoot, mirrorRoot, revisionKey string, deployments []*Config) ([]*Config, error) {
	expanded := make([]*Config, 0, len(deployments))

	if revisionKey == "" {
		revisionKey = revisionKeyForRepoRoot(repoRoot)
	}

	for _, deployment := range deployments {
		if !deployment.AutoDiscovery.Enabled {
			expanded = append(expanded, deployment)
			continue
		}

		fsys, release := publishedGitDiscoveryFS(repoRoot, labelRoot, mirrorRoot, plumbing.NewHash(revisionKey), deployment)
		discoveredConfigs, err := autoDiscoverDeployments(fsys, labelRoot, revisionKey, deployment)

		if release != nil {
			release()
		}

		if err != nil {
			return nil, fmt.Errorf("failed to auto-discover deployment configurations: %w", err)
		}

		expanded = append(expanded, discoveredConfigs...)
	}

	return expanded, nil
}

// publishedGitDiscoveryFS uses the object tree only when the published disk
// artifact has been proved equivalent for discovery.
// The caller releases the mirror read lock after scanning the returned TreeFS.
func publishedGitDiscoveryFS(repoRoot, labelRoot, mirrorRoot string, revision plumbing.Hash, base *Config) (fs.FS, func()) {
	disk := os.DirFS(repoRoot)
	if revision.IsZero() || !isPublishedPrimaryGitArtifact(repoRoot, mirrorRoot, revision) {
		return disk, nil
	}

	unlock := sourcecache.AcquireSharedPathLock(mirrorRoot)

	repo, err := git.PlainOpen(mirrorRoot)
	if err != nil {
		unlock()
		slog.Debug("could not open discovery mirror; using published artifact", "reason", err)

		return disk, nil
	}

	tree, err := gitInternal.NewTreeFSAtCommit(repo, revision)
	if err != nil {
		unlock()
		slog.Debug("could not open discovery tree; using published artifact", "reason", err)

		return disk, nil
	}

	if !plainGitDiscoveryTree(tree, disk, labelRoot, base) {
		unlock()
		return disk, nil
	}

	return tree, unlock
}

// revisionKeyForRepoRoot returns the current HEAD commit hash for repoRoot,
// or "" if repoRoot is not a git repository. A disk scan never uses Git tree
// hashes for subtree caching: materialized contents may differ from HEAD.
func revisionKeyForRepoRoot(repoRoot string) string {
	repo, err := git.PlainOpen(repoRoot)
	if err != nil {
		return ""
	}

	head, err := repo.Head()
	if err != nil {
		return ""
	}

	return head.Hash().String()
}

// autoDiscoverDeployments scans fsys for compose files and creates a Config for each matching subdirectory.
// Only an object-backed TreeFS with a matching revision can reuse Git subtree metadata;
// disk artifacts, submodules and OCI sources cannot be identified by Git tree hashes.
func autoDiscoverDeployments(fsys fs.FS, repoRoot, revisionKey string, baseConfig *Config) ([]*Config, error) {
	start := time.Now()
	repositoryLabel := filepath.Base(filepath.Clean(repoRoot))
	scanner := discoveryScanner{
		fsys:       fsys,
		repository: repoRoot,
		label:      repositoryLabel,
		base:       baseConfig,
		compose:    set.New(baseConfig.ComposeFiles...),
	}

	// A TreeFS never materializes submodules or decrypted files. Do not use this
	// optimization for os.DirFS, even when its path names a published Git revision.
	if tree, ok := fsys.(gitTreeDiscoveryFS); ok && revisionKey != "" && tree.Commit().String() == revisionKey {
		names := append([]string(nil), baseConfig.ComposeFiles...)
		sort.Strings(names)
		settings, _ := json.Marshal(struct {
			ComposeFiles []string
			ConfigFiles  []string
		}{names, DefaultDeploymentConfigFileNames})
		scanner.tree, scanner.settings = tree, string(settings)
	}

	mode := "tree"
	if scanner.tree == nil {
		mode = "full"

		recordAutoDiscoveryCacheLookup(repositoryLabel, "bypass")
	}

	defer func() {
		slog.Debug("auto-discovery scan", "mode", mode, "duration", time.Since(start),
			"directory_reads", scanner.directoryReads, "cache_hits", scanner.cacheHits, "cache_misses", scanner.cacheMisses)
	}()

	searchPath := path.Clean(baseConfig.WorkingDirectory)

	info, err := fs.Stat(fsys, searchPath)
	if err != nil {
		return nil, err
	}

	if !info.IsDir() {
		return nil, nil
	}

	if _, err := scanner.scan(searchPath, 0); err != nil {
		return nil, err
	}

	return scanner.configs, nil
}

type gitTreeDiscoveryFS interface {
	fs.FS
	Commit() plumbing.Hash
	SubtreeHash(string) (plumbing.Hash, error)
}

// Ensure gitInternal.TreeFS implements gitTreeDiscoveryFS.
var _ gitTreeDiscoveryFS = (*gitInternal.TreeFS)(nil)

// plainGitDiscoveryTree compares directory inventories and nested config bytes
// against a GitStore-published artifact. Gitlinks become directories, symlinks
// redirect reads, and SOPS can replace nested configs: none can use TreeFS.
// Positive proofs transfer to other immutable artifacts with identical Git
// subtree hashes; unproven or failed subtrees always fall back to disk.
// Reuse relies on GitStore's contract that published artifacts are not modified
// after verification; Git hashes cannot detect external writes to those paths.
func plainGitDiscoveryTree(tree gitTreeDiscoveryFS, disk fs.FS, repository string, base *Config) bool {
	root := path.Clean(base.WorkingDirectory)
	if !fs.ValidPath(root) {
		return false
	}

	settings, _ := json.Marshal(struct {
		ConfigFiles []string
		Root        string
		Depth       int
	}{DefaultDeploymentConfigFileNames, root, base.AutoDiscovery.ScanDepth})

	return provePlainGitSubtree(tree, disk, ".", root, base.AutoDiscovery.ScanDepth,
		sha256.Sum256([]byte(repository)), sha256.Sum256(settings))
}

// provePlainGitSubtree recursively compares a Git subtree against a disk artifact.
// It returns true if the subtree is proven equivalent to the disk artifact, false otherwise.
// The proof is cached by subtree hash, repository hash, and settings hash.
func provePlainGitSubtree(tree gitTreeDiscoveryFS, disk fs.FS, p, root string, depth int, repository, settings [sha256.Size]byte) bool {
	hash, err := tree.SubtreeHash(p)
	if err != nil {
		return false
	}

	key := discoveryProofKey{tree: hash, repository: repository, settings: settings}
	if autoDiscoveryProof.get(key) {
		return true
	}

	entries, err := fs.ReadDir(tree, p)
	if err != nil {
		return false
	}

	diskEntries, err := fs.ReadDir(disk, p)
	if err != nil || len(entries) != len(diskEntries) {
		return false
	}

	for i, entry := range entries {
		// TreeFS marks gitlinks irregular. ExportTree turns even an unfetched
		// gitlink into a directory, so skipping it would lose disk inventory.
		if entry.Name() != diskEntries[i].Name() || entry.IsDir() != diskEntries[i].IsDir() ||
			(entry.Type()&fs.ModeType != 0 && !entry.IsDir()) ||
			(diskEntries[i].Type()&fs.ModeType != 0 && !diskEntries[i].IsDir()) {
			return false
		}

		child := path.Join(p, entry.Name())
		if entry.IsDir() {
			if !discoveryProofVisits(child, entry.Name(), root, depth) {
				continue
			}

			if !provePlainGitSubtree(tree, disk, child, root, depth, repository, settings) {
				return false
			}

			continue
		}

		for _, name := range DefaultDeploymentConfigFileNames {
			if entry.Name() != name {
				continue
			}

			contents, readErr := fs.ReadFile(tree, child)
			if readErr != nil {
				return false
			}

			diskContents, readErr := fs.ReadFile(disk, child)
			if readErr != nil || !bytes.Equal(contents, diskContents) {
				return false
			}

			if _, encrypted := encryption.DetectFormat(contents, child); encrypted {
				return false
			}
		}
	}

	autoDiscoveryProof.put(key)

	return true
}

// discoveryProofVisits determines whether a child directory should be visited during proof verification.
// It returns true if the child is within the root directory and within the specified depth limit.
func discoveryProofVisits(child, name, root string, depth int) bool {
	if root != "." {
		if child == root || strings.HasPrefix(root, child+"/") {
			return true
		}

		if !strings.HasPrefix(child, root+"/") {
			return false
		}
	}

	if filesystem.IsIgnoredDir(name) {
		return false
	}

	if depth == 0 {
		return true
	}

	rel := child
	if root != "." {
		rel = strings.TrimPrefix(child, root+"/")
	}

	return strings.Count(rel, "/")+1 <= depth
}

// discoveryScanner scans a filesystem for docker-compose files
// and creates Configs for each matching subdirectory.
type discoveryScanner struct {
	fsys       fs.FS
	tree       gitTreeDiscoveryFS
	repository string
	label      string
	settings   string
	base       *Config
	compose    set.Set[string]
	configs    []*Config

	directoryReads, cacheHits, cacheMisses int
}

// appendConfig clones the base Config, applies any overrides from the discovery match,
// and appends it to the scanner's configs slice. It returns an error if the resulting
// Config is invalid.
func (s *discoveryScanner) appendConfig(p string, match discoveryMatch) error {
	c := clone.New(s.base)

	stackDirName := path.Base(p)
	if p == "." {
		stackDirName = s.label
	}

	if s.base.Name != "" && stackDirName == s.label {
		c.Name = s.base.Name
	} else {
		c.Name = stackDirName
	}

	c.WorkingDirectory = p
	if match.override != nil {
		mergeConfig(c, clone.New(match.override))
	}

	if err := c.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}

	s.configs = append(s.configs, c)

	return nil
}

// scan visits directories in fs.WalkDir's lexicographic, parent-first order.
// Each visited directory is read only once, for both matches and child traversal.
func (s *discoveryScanner) scan(p string, depth int) ([]discoveryMatch, error) {
	var key discoveryCacheKey

	if s.tree != nil {
		hash, err := s.tree.SubtreeHash(p)
		if err != nil {
			return nil, err
		}

		remaining := -1
		if s.base.AutoDiscovery.ScanDepth > 0 {
			remaining = s.base.AutoDiscovery.ScanDepth - depth
		}

		key = discoveryCacheKey{s.repository, hash.String(), s.settings, remaining}
		if matches, ok := autoDiscoveryCache.get(key); ok {
			s.cacheHits++
			recordAutoDiscoveryCacheLookup(s.label, "hit")

			for _, match := range matches {
				if err := s.appendConfig(path.Join(p, match.dir), match); err != nil {
					return nil, err
				}
			}

			return matches, nil
		}

		s.cacheMisses++
		recordAutoDiscoveryCacheLookup(s.label, "miss")
	}

	entries, err := fs.ReadDir(s.fsys, p)
	if err != nil {
		return nil, err
	}

	s.directoryReads++

	var matches []discoveryMatch

	if dirContainsAnyComposeFile(entries, s.compose) {
		match := discoveryMatch{dir: "."}

		// A local .yaml takes precedence over .yml, and only a directory with
		// a matching compose file reads or validates its nested config.
		for _, cfgName := range DefaultDeploymentConfigFileNames {
			if !dirHasFile(entries, cfgName) {
				continue
			}

			localCfgPath := path.Join(p, cfgName)

			b, readErr := fs.ReadFile(s.fsys, localCfgPath)
			if readErr != nil {
				return nil, fmt.Errorf("failed to read nested .doco-cd config at %s: %w", localCfgPath, readErr)
			}

			localConfigs, parseErr := getConfigFromYAMLBytes(b, localCfgPath, false)
			if parseErr != nil {
				return nil, fmt.Errorf("failed to parse nested .doco-cd config at %s: %w", localCfgPath, parseErr)
			}

			if len(localConfigs) > 1 {
				return nil, fmt.Errorf("%w: %s contains %d documents", ErrMultipleYAMLDocuments, localCfgPath, len(localConfigs))
			}

			match.override = localConfigs[0]
			match.override.Internal.File = ""

			// Bound cached contents by the size of the parsed value, not only
			// the source bytes (YAML aliases can expand during decoding).
			if s.tree != nil {
				encoded, marshalErr := json.Marshal(match.override)
				if marshalErr != nil {
					match.overrideSize = maxAutoDiscoveryCacheEntryBytes + 1
				} else {
					match.overrideSize = len(encoded) * 4
				}
			}

			break
		}

		if err := s.appendConfig(p, match); err != nil {
			return nil, err
		}

		matches = append(matches, match)
	}

	for _, entry := range entries {
		if !entry.IsDir() || filesystem.IsIgnoredDir(entry.Name()) ||
			(s.base.AutoDiscovery.ScanDepth > 0 && depth >= s.base.AutoDiscovery.ScanDepth) {
			continue
		}

		child, err := s.scan(path.Join(p, entry.Name()), depth+1)
		if err != nil {
			return nil, err
		}

		for _, match := range child {
			match.dir = path.Join(entry.Name(), match.dir)
			matches = append(matches, match)
		}
	}

	if s.tree != nil {
		autoDiscoveryCache.put(key, matches)
	}

	return matches, nil
}

// cloneConfigSlice creates a deep copy of a slice of Config pointers.
func cloneConfigSlice(configs []*Config) []*Config {
	if configs == nil {
		return nil
	}

	cloned := make([]*Config, 0, len(configs))

	for _, cfg := range configs {
		if cfg == nil {
			cloned = append(cloned, nil)
			continue
		}

		cloned = append(cloned, clone.New(cfg))
	}

	return cloned
}

// dirContainsAnyComposeFile returns true when a compose filename is present as a
// regular file in the pre-read directory entries.
func dirContainsAnyComposeFile(entries []os.DirEntry, composeFileNames set.Set[string]) bool {
	if len(entries) == 0 || len(composeFileNames) == 0 {
		return false
	}

	for _, entry := range entries {
		if !entry.IsDir() && composeFileNames.Contains(entry.Name()) {
			return true
		}
	}

	return false
}

// dirHasFile returns true when the given filename exists as a non-directory
// entry in the pre-read slice.
func dirHasFile(entries []os.DirEntry, name string) bool {
	for _, entry := range entries {
		if !entry.IsDir() && entry.Name() == name {
			return true
		}
	}

	return false
}

// mergeConfig merges Config fields from override into base, but only for fields
// tagged with `doco:"allowOverride"`. Protected fields remain unchanged.
// Merge semantics:
//   - Maps: merged key-by-key (override wins on key collision)
//   - Slices: replaced entirely if the override slice is non-empty
//   - Nested structs: all sub-fields are merged (parent tag opts them in)
//   - Scalars: replaced if the override holds a non-zero value.
func mergeConfig(base, override *Config) {
	mergeStructByTag(reflect.ValueOf(base).Elem(), reflect.ValueOf(override).Elem())
}

// mergeStructByTag iterates a struct's fields and merges only those tagged doco:"allowOverride".
func mergeStructByTag(base, override reflect.Value) {
	t := base.Type()
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Tag.Get("doco") != "allowOverride" {
			continue
		}

		mergeField(base.Field(i), override.Field(i))
	}
}

// mergeField applies a single field merge from override into base.
// For structs the merge recurses into all sub-fields (no tag check – parent has opted in).
func mergeField(base, override reflect.Value) {
	switch base.Kind() {
	case reflect.Map:
		if override.IsNil() || override.Len() == 0 {
			return
		}

		if base.IsNil() {
			base.Set(reflect.MakeMap(base.Type()))
		}

		for _, k := range override.MapKeys() {
			base.SetMapIndex(k, override.MapIndex(k))
		}

	case reflect.Slice:
		if override.IsNil() || override.Len() == 0 {
			return
		}

		base.Set(override)

	case reflect.Struct:
		// Recurse into all sub-fields; the parent tag already opted them in.
		mergeAllStructFields(base, override)

	default:
		// Scalar: apply only when the override holds a non-zero value.
		if !override.IsZero() {
			base.Set(override)
		}
	}
}

// mergeAllStructFields merges every field of override into base without tag checks.
func mergeAllStructFields(base, override reflect.Value) {
	for i := 0; i < base.NumField(); i++ {
		mergeField(base.Field(i), override.Field(i))
	}
}
