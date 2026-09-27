package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/source/store"
)

const (
	// composeLiveRootDir is the directory of a stack's live directory holding the live copies.
	composeLiveRootDir = "root"
	// composeLiveManifestFile records the entries of the live copies that were copied from an
	// artifact, so entries that were removed from the repository can be removed from the live
	// copies without touching files that were written by the services themselves.
	composeLiveManifestFile = "manifest.json"
)

// composeLiveManifest is the content of composeLiveManifestFile.
type composeLiveManifest struct {
	// Entries are the slash-separated paths, relative to the live root, of all copied entries.
	Entries []string `json:"entries"`
}

// composeLiveResources are the repository files of a compose project that are excluded from
// recreation with the recreate.ignore label and therefore served from a live copy.
//
// Every revision is deployed from its own immutable artifact directory, so a service that is
// not recreated keeps using the files of the artifact it was created from. Services that should
// pick up changes of these files without being recreated (e.g. by reloading them on a signal or
// by watching them) therefore mount them from a live copy in the stack's live directory
// ("<store>/live/<context>/<project>/root") instead, which is updated in place on each
// deployment.
type composeLiveResources struct {
	artifactRoot string
	// dir is the stack's live directory, root the directory of the live copies within it.
	dir, root string
	// resources are the slash-separated repository paths that are served from the live copy,
	// without paths nested in another resource.
	resources []string
	// services are the repository paths each service mounts from the live copy.
	services map[string][]string
	// signals are the recreate.ignore signals of the services that mount live copies.
	signals map[string]string
}

// composeLiveDir returns the live directory of a stack in the store at storeBase.
func composeLiveDir(storeBase, contextName, projectName string) (string, error) {
	contextName = DisplayContextName(contextName)

	for _, name := range []string{contextName, projectName} {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			return "", fmt.Errorf("invalid live directory name %q", name)
		}
	}

	return filepath.Join(storeBase, store.LiveSubdir, contextName, projectName), nil
}

// prepareComposeLiveResources rewrites the host paths of all repository files that are excluded
// from recreation with the recreate.ignore label to their live copies and labels the affected
// services with the live repository paths they use (see DocoCDLabels.Deployment.LiveResources),
// so a service is recreated once when it starts using a live copy, or a different set of them.
//
// Bind mounts are rewritten when their target is ignored in the bindMounts scope, file-based
// configs and secrets when a service using them ignores them in the configs or secrets scope.
// Paths that already point to the live copy are accepted as well.
//
// It returns nil if project was not loaded from an artifact.
func prepareComposeLiveResources(project *types.Project, artifactRoot, contextName string) (*composeLiveResources, error) {
	if artifactRoot == "" {
		return nil, nil
	}

	artifactRoot = filepath.Clean(artifactRoot)

	artifactsDir := filepath.Dir(artifactRoot)
	if filepath.Base(artifactsDir) != store.ArtifactsSubdir || !filesystem.IsDir(artifactRoot) {
		return nil, nil
	}

	dir, err := composeLiveDir(filepath.Dir(artifactsDir), contextName, project.Name)
	if err != nil {
		return nil, err
	}

	ignoreCfg, err := getIgnoreRecreateCfgFromProject(project)
	if err != nil {
		return nil, err
	}

	live := &composeLiveResources{
		artifactRoot: artifactRoot,
		dir:          dir,
		root:         filepath.Join(dir, composeLiveRootDir),
		services:     make(map[string][]string),
		signals:      make(map[string]string),
	}

	var all []string

	for _, name := range slices.Sorted(maps.Keys(ignoreCfg)) {
		service, ok := project.Services[name]
		if !ok {
			continue
		}

		rels := live.rewriteService(project, &service, ignoreCfg[name].ignoreMap)
		if len(rels) == 0 {
			continue
		}

		slices.Sort(rels)
		rels = slices.Compact(rels)

		service.Labels = maps.Clone(service.Labels)
		if service.Labels == nil {
			service.Labels = types.Labels{}
		}

		service.Labels[DocoCDLabels.Deployment.LiveResources] = strings.Join(rels, ",")
		project.Services[name] = service

		live.services[name] = rels
		if signal := ignoreCfg[name].signal; signal != "" {
			live.signals[name] = signal
		}

		all = append(all, rels...)
	}

	live.resources = outermostRelPaths(all)

	return live, nil
}

// rewriteService rewrites the ignored repository paths used by service to their live copies and
// returns the repository paths it rewrote.
func (l *composeLiveResources) rewriteService(project *types.Project, service *types.ServiceConfig, ignore ignoreCfg) []string {
	var rels []string

	if rule, ok := ignore[changeScopeBindMounts]; ok {
		volumes := slices.Clone(service.Volumes)

		for i, volume := range volumes {
			if volume.Type != types.VolumeTypeBind || !rule.IsIgnore(volume.Target) {
				continue
			}

			if rel, ok := l.repoRel(volume.Source); ok {
				volumes[i].Source = l.livePath(rel)
				rels = append(rels, rel)
			}
		}

		if len(rels) > 0 {
			service.Volumes = volumes
		}
	}

	if rule, ok := ignore[changeScopeConfigs]; ok {
		for _, ref := range service.Configs {
			definition, defined := project.Configs[ref.Source]
			if !defined || bool(definition.External) || !rule.IsIgnore(ref.Source) {
				continue
			}

			if rel, ok := l.repoRel(definition.File); ok {
				definition.File = l.livePath(rel)
				project.Configs[ref.Source] = definition

				rels = append(rels, rel)
			}
		}
	}

	if rule, ok := ignore[changeScopeSecrets]; ok {
		for _, ref := range service.Secrets {
			definition, defined := project.Secrets[ref.Source]
			if !defined || bool(definition.External) || !rule.IsIgnore(ref.Source) {
				continue
			}

			if rel, ok := l.repoRel(definition.File); ok {
				definition.File = l.livePath(rel)
				project.Secrets[ref.Source] = definition

				rels = append(rels, rel)
			}
		}
	}

	return rels
}

// repoRel returns the repository path of a host path into the artifact or into the live copy.
func (l *composeLiveResources) repoRel(path string) (string, bool) {
	if rel, ok := relPathWithin(l.artifactRoot, path); ok {
		return rel, true
	}

	return relPathWithin(l.root, path)
}

// livePath returns the host path of the live copy of the repository path rel.
func (l *composeLiveResources) livePath(rel string) string {
	return filepath.Join(l.root, filepath.FromSlash(rel))
}

// sync updates the live copies from the artifact and returns the repository paths of all entries
// it created, modified or removed. When full is false, existing live copies are left untouched
// and only missing ones are created, e.g. when services are recreated from the already deployed
// revision.
//
// Entries that exist in the live copy but not in the artifact are only removed if they were
// copied from an artifact before, so files written by the services themselves are kept.
func (l *composeLiveResources) sync(full bool) ([]string, error) {
	if len(l.resources) == 0 {
		return nil, nil
	}

	manifest, err := l.readManifest()
	if err != nil {
		return nil, err
	}

	var managed, changed []string

	for _, resource := range l.resources {
		previous := slices.DeleteFunc(slices.Clone(manifest.Entries), func(entry string) bool {
			return !relPathContains(resource, entry)
		})

		dst := l.livePath(resource)

		if !full {
			if _, err = os.Lstat(dst); err == nil {
				managed = append(managed, previous...)

				continue
			} else if !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("stat %s: %w", dst, err)
			}
		}

		if err = os.MkdirAll(filepath.Dir(dst), filesystem.PermDir); err != nil {
			return nil, fmt.Errorf("create live directory: %w", err)
		}

		entries, resourceChanged, err := filesystem.SyncInPlace(filepath.Join(l.artifactRoot, filepath.FromSlash(resource)), dst)
		if err != nil {
			return nil, fmt.Errorf("update live copy of %s: %w", resource, err)
		}

		for i, entry := range entries {
			entries[i] = joinRelPath(resource, entry)
		}

		for _, entry := range resourceChanged {
			changed = append(changed, joinRelPath(resource, entry))
		}

		managed = append(managed, entries...)

		if !full {
			continue
		}

		stale := slices.DeleteFunc(previous, func(entry string) bool { return slices.Contains(entries, entry) })

		// Stale directories are entries themselves, and their parents may still be current entries.
		removed, kept := l.removeEntries(stale, false)
		changed = append(changed, removed...)
		managed = append(managed, kept...)
	}

	// Entries outside of the current resources are removed by prune once no container uses them.
	for _, entry := range manifest.Entries {
		if !slices.ContainsFunc(l.resources, func(resource string) bool { return relPathContains(resource, entry) }) {
			managed = append(managed, entry)
		}
	}

	if err = l.writeManifest(managed); err != nil {
		return nil, err
	}

	slices.Sort(changed)

	return slices.Compact(changed), nil
}

// prune removes the live copies that are neither used by the current resources nor mounted by
// any container of the project, and the live directory itself once it is empty.
func (l *composeLiveResources) prune(ctx context.Context, apiClient client.APIClient, projectName string) error {
	manifest, err := l.readManifest()
	if err != nil {
		return err
	}

	if len(manifest.Entries) == 0 && len(l.resources) == 0 {
		return l.removeDirs()
	}

	containers, err := composeServiceContainers(ctx, apiClient, projectName)
	if err != nil {
		return fmt.Errorf("list containers of project %s: %w", projectName, err)
	}

	inUse := slices.Clone(l.resources)

	for _, serviceContainers := range containers {
		for _, c := range serviceContainers {
			for _, m := range c.Mounts {
				if rel, ok := relPathWithin(l.root, m.Source); ok && m.Type == mount.TypeBind {
					inUse = append(inUse, rel)
				}
			}
		}
	}

	var kept, unused []string

	for _, entry := range manifest.Entries {
		if slices.ContainsFunc(inUse, func(rel string) bool { return relPathContains(rel, entry) || relPathContains(entry, rel) }) {
			kept = append(kept, entry)
		} else {
			unused = append(unused, entry)
		}
	}

	_, notRemoved := l.removeEntries(unused, true)
	kept = append(kept, notRemoved...)

	if len(kept) > 0 || len(l.resources) > 0 {
		return l.writeManifest(kept)
	}

	return l.removeDirs()
}

// removeDirs removes the manifest and the live directory of the stack if it is empty otherwise.
// The parent directories are shared with other stacks and therefore kept.
func (l *composeLiveResources) removeDirs() error {
	if err := os.Remove(filepath.Join(l.dir, composeLiveManifestFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove live manifest: %w", err)
	}

	// Removing the directories fails if they still contain anything, e.g. directories created
	// by Docker for missing bind mount sources, which is fine.
	for _, dir := range []string{l.root, l.dir} {
		if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			break
		}
	}

	return nil
}

// removeEntries removes the live copies of the entries, deepest first, and, if removeParents is
// set, the directories that became empty by that up to the live root. Directories are only
// removed if they are empty. It returns the removed entries and the entries that could not be
// removed.
func (l *composeLiveResources) removeEntries(entries []string, removeParents bool) (removed, kept []string) {
	entries = slices.Clone(entries)
	slices.SortFunc(entries, func(a, b string) int {
		return strings.Compare(b, a)
	})

	for _, entry := range slices.Compact(entries) {
		path := l.livePath(entry)

		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			kept = append(kept, entry)

			continue
		}

		removed = append(removed, entry)

		if !removeParents {
			continue
		}

		for dir := filepath.Dir(path); dir != l.root && strings.HasPrefix(dir, l.root+string(filepath.Separator)); dir = filepath.Dir(dir) {
			if err := os.Remove(dir); err != nil {
				break
			}
		}
	}

	return removed, kept
}

func (l *composeLiveResources) readManifest() (composeLiveManifest, error) {
	var manifest composeLiveManifest

	content, err := os.ReadFile(filepath.Join(l.dir, composeLiveManifestFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return manifest, nil
		}

		return manifest, fmt.Errorf("read live manifest: %w", err)
	}

	if err = json.Unmarshal(content, &manifest); err != nil {
		return manifest, fmt.Errorf("parse live manifest: %w", err)
	}

	// Never trust paths that would lead out of the live root.
	manifest.Entries = slices.DeleteFunc(manifest.Entries, func(entry string) bool {
		rel, ok := relPathWithin(l.root, l.livePath(entry))
		return !ok || rel != entry
	})

	return manifest, nil
}

func (l *composeLiveResources) writeManifest(entries []string) error {
	entries = slices.Clone(entries)
	slices.Sort(entries)

	content, err := json.Marshal(composeLiveManifest{Entries: slices.Compact(entries)})
	if err != nil {
		return fmt.Errorf("encode live manifest: %w", err)
	}

	if err = os.MkdirAll(l.dir, filesystem.PermDir); err != nil {
		return fmt.Errorf("create live directory: %w", err)
	}

	tmp, err := os.CreateTemp(l.dir, "."+composeLiveManifestFile+"-*")
	if err != nil {
		return fmt.Errorf("create live manifest: %w", err)
	}

	defer func() { _ = os.Remove(tmp.Name()) }()

	_, err = tmp.Write(content)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}

	if err != nil {
		return fmt.Errorf("write live manifest: %w", err)
	}

	if err = os.Rename(tmp.Name(), filepath.Join(l.dir, composeLiveManifestFile)); err != nil {
		return fmt.Errorf("write live manifest: %w", err)
	}

	return nil
}

// changedServices returns the services that mount any of the changed repository paths.
func (l *composeLiveResources) changedServices(changed []string) []string {
	var services []string

	for _, name := range slices.Sorted(maps.Keys(l.services)) {
		if slices.ContainsFunc(l.services[name], func(rel string) bool {
			return slices.ContainsFunc(changed, func(path string) bool {
				return relPathContains(rel, path) || relPathContains(path, rel)
			})
		}) {
			services = append(services, name)
		}
	}

	return services
}

// signals returns the signals to send to the services whose live copies changed, so they reload
// them. Services that are already signaled or that are force-recreated are skipped, as are
// services without a running container that mounts a live copy, since those do not use the live
// copy yet and are recreated anyway.
func (l *composeLiveResources) signalsFor(
	ctx context.Context,
	apiClient client.APIClient,
	projectName string,
	changed []string,
	signaled []SignalService,
	recreateMode string,
	forcedServices []string,
) ([]SignalService, error) {
	if len(changed) == 0 || len(l.signals) == 0 || (recreateMode == api.RecreateForce && len(forcedServices) == 0) {
		return nil, nil
	}

	var candidates []string

	for _, name := range l.changedServices(changed) {
		if l.signals[name] == "" ||
			slices.ContainsFunc(signaled, func(s SignalService) bool { return s.ServiceName == name }) ||
			(recreateMode == api.RecreateForce && slices.Contains(forcedServices, name)) {
			continue
		}

		candidates = append(candidates, name)
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	containers, err := composeServiceContainers(ctx, apiClient, projectName)
	if err != nil {
		return nil, fmt.Errorf("list containers of project %s: %w", projectName, err)
	}

	var signals []SignalService

	for _, name := range candidates {
		if slices.ContainsFunc(containers[name], l.runningWithLiveMount) {
			signals = append(signals, SignalService{ServiceName: name, Signal: l.signals[name]})
		}
	}

	return signals, nil
}

// runningWithLiveMount reports whether c is running and mounts a live copy of its stack.
func (l *composeLiveResources) runningWithLiveMount(c container.Summary) bool {
	if c.State != container.StateRunning {
		return false
	}

	return slices.ContainsFunc(c.Mounts, func(m container.MountPoint) bool {
		_, ok := relPathWithin(l.root, m.Source)
		return ok && m.Type == mount.TypeBind
	})
}

// removeComposeLiveDirs removes the live directories of a stack that are mounted by its containers.
func removeComposeLiveDirs(containers map[string][]container.Summary, projectName string) error {
	var errs []error

	for _, dir := range composeLiveDirsOfContainers(containers, projectName) {
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("remove live directory %s: %w", dir, err))
		}
	}

	return errors.Join(errs...)
}

// composeLiveDirsOfContainers returns the live directories of a stack that are mounted by its
// containers ("<store>/live/<context>/<project>"), in any context.
func composeLiveDirsOfContainers(containers map[string][]container.Summary, projectName string) []string {
	var dirs []string

	for _, serviceContainers := range containers {
		for _, c := range serviceContainers {
			for _, m := range c.Mounts {
				if m.Type != mount.TypeBind || !filepath.IsAbs(m.Source) {
					continue
				}

				if dir, ok := composeLiveDirOfPath(m.Source, projectName); ok {
					dirs = append(dirs, dir)
				}
			}
		}
	}

	slices.Sort(dirs)

	return slices.Compact(dirs)
}

// composeLiveDirOfPath returns the live directory of the stack projectName that contains path.
func composeLiveDirOfPath(path, projectName string) (string, bool) {
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))

	for i := 1; i+3 < len(parts); i++ {
		if parts[i] != store.LiveSubdir || parts[i+2] != projectName || parts[i+3] != composeLiveRootDir {
			continue
		}

		storeBase := string(filepath.Separator) + filepath.Join(parts[:i]...)
		dir := filepath.Join(storeBase, filepath.Join(parts[i:i+3]...))

		// Only accept directories that really are live directories of a store.
		if !filesystem.IsDir(filepath.Join(storeBase, store.ArtifactsSubdir)) || !filesystem.IsDir(filepath.Join(dir, composeLiveRootDir)) {
			return "", false
		}

		return dir, true
	}

	return "", false
}

// relPathWithin returns the slash-separated path of path relative to base, if path is base or
// is located in base.
func relPathWithin(base, path string) (string, bool) {
	if path == "" || !filepath.IsAbs(path) {
		return "", false
	}

	rel, err := filepath.Rel(base, filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}

	return filepath.ToSlash(rel), true
}

// relPathContains reports whether the slash-separated relative path child is parent or is
// located in parent.
func relPathContains(parent, child string) bool {
	return parent == "." || child == parent || strings.HasPrefix(child, parent+"/")
}

// joinRelPath joins the slash-separated relative paths base and rel.
func joinRelPath(base, rel string) string {
	switch {
	case rel == ".":
		return base
	case base == ".":
		return rel
	default:
		return base + "/" + rel
	}
}

// outermostRelPaths returns the sorted, unique paths that are not located in another of the paths.
func outermostRelPaths(paths []string) []string {
	paths = slices.Clone(paths)
	slices.Sort(paths)

	var result []string

	for _, path := range slices.Compact(paths) {
		if !slices.ContainsFunc(result, func(parent string) bool { return relPathContains(parent, path) }) {
			result = append(result, path)
		}
	}

	return result
}
