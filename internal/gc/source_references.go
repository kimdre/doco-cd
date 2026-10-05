package gc

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/docker"
)

// sourceReferenceClient is the read-only part of the Docker API that source references are collected with.
type sourceReferenceClient interface {
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	VolumeInspect(ctx context.Context, volumeID string, options client.VolumeInspectOptions) (client.VolumeInspectResult, error)
	ServiceList(ctx context.Context, options client.ServiceListOptions) (client.ServiceListResult, error)
	TaskList(ctx context.Context, options client.TaskListOptions) (client.TaskListResult, error)
}

// sourceReferences is everything that the containers, Swarm services and Swarm tasks of every Docker context
// reference of the source stores: the paths below the data directory they mount or record in their labels, and the
// sources named by their doco-cd labels.
//
// A path references every store it is in and every store below it. Paths outside the data directory, the data
// directory itself (which doco-cd mounts) and its ancestors (which e.g. monitoring agents mount) reference nothing.
type sourceReferences struct {
	// roots are the data directory's paths on the host and in this container.
	roots []string
	// paths are the referenced paths relative to the data directory, slash-separated.
	paths set.Set[string]
	// repositories are the referenced sources, in the form of the live revision keys (see LiveRevisions).
	repositories set.Set[string]
}

func newSourceReferences(roots ...string) *sourceReferences {
	refs := &sourceReferences{paths: set.New[string](), repositories: set.New[string]()}

	for _, root := range roots {
		if root = strings.TrimSpace(root); root != "" && filepath.IsAbs(root) {
			refs.roots = append(refs.roots, filepath.Clean(root))
		}
	}

	return refs
}

// collectSourceReferences collects what every configured Docker context references of the source stores below the
// data directory, whose path is dataMountSource on the host and dataMountDestination in this container.
//
// Collection fails closed: if any context, container, service, task or volume cannot be inspected, or a volume's
// bind source cannot be determined, the stores could still be in use and none may be evicted.
func collectSourceReferences(ctx context.Context, contexts *docker.ContextRegistry, dataMountSource, dataMountDestination string) (*sourceReferences, error) {
	if contexts == nil {
		return nil, errors.New("docker context registry is unavailable")
	}

	results, err := contexts.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list docker contexts: %w", err)
	}

	return sourceReferencesOf(ctx, results, dataMountSource, dataMountDestination)
}

// sourceReferencesOf collects what the Docker contexts in results reference of the source stores, failing if any of
// them is unavailable.
func sourceReferencesOf(ctx context.Context, results []docker.ContextClientResult, dataMountSource, dataMountDestination string) (*sourceReferences, error) {
	refs := newSourceReferences(dataMountSource, dataMountDestination)

	for _, result := range results {
		if result.Err != nil {
			return nil, fmt.Errorf("inspect docker context %s: %w", result.DisplayName(), result.Err)
		}

		if result.Cli == nil {
			return nil, fmt.Errorf("inspect docker context %s: missing client", result.DisplayName())
		}

		if err := refs.addContext(ctx, result.Cli.Client(), result.SwarmMode); err != nil {
			return nil, fmt.Errorf("inspect docker context %s: %w", result.DisplayName(), err)
		}
	}

	return refs, nil
}

// addContext adds the references of every container of a Docker context, running or not and labeled or not, and in
// Swarm mode of every service, including its previous specification during a rolling update, and every task.
func (r *sourceReferences) addContext(ctx context.Context, apiClient sourceReferenceClient, swarmMode bool) error {
	volumes := make(map[string][]string)

	containers, err := apiClient.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}

	for _, cont := range containers.Items {
		r.addLabels(cont.Labels)

		for _, m := range cont.Mounts {
			switch m.Type {
			case mount.TypeBind:
				r.addPath(m.Source)
			case mount.TypeVolume:
				// The volume's storage location, which volume plugins may place anywhere on the host.
				r.addPath(m.Source)

				if m.Driver != "" && m.Driver != "local" {
					continue
				}

				if err := r.addVolume(ctx, apiClient, volumes, m.Name); err != nil {
					return fmt.Errorf("inspect container %s: %w", cont.ID, err)
				}
			}
		}
	}

	if !swarmMode {
		return nil
	}

	services, err := apiClient.ServiceList(ctx, client.ServiceListOptions{})
	if err != nil {
		return fmt.Errorf("list swarm services: %w", err)
	}

	for _, service := range services.Items {
		specs := []*swarm.ServiceSpec{&service.Spec}
		if service.PreviousSpec != nil {
			specs = append(specs, service.PreviousSpec)
		}

		for _, spec := range specs {
			r.addLabels(spec.Labels)

			if err := r.addTaskSpec(ctx, apiClient, volumes, spec.TaskTemplate); err != nil {
				return fmt.Errorf("inspect swarm service %s: %w", service.Spec.Name, err)
			}
		}
	}

	// Tasks of every state: during a rolling update, old tasks still run with the mounts of an earlier specification.
	tasks, err := apiClient.TaskList(ctx, client.TaskListOptions{})
	if err != nil {
		return fmt.Errorf("list swarm tasks: %w", err)
	}

	for _, task := range tasks.Items {
		if err := r.addTaskSpec(ctx, apiClient, volumes, task.Spec); err != nil {
			return fmt.Errorf("inspect swarm task %s: %w", task.ID, err)
		}
	}

	return nil
}

func (r *sourceReferences) addTaskSpec(ctx context.Context, apiClient sourceReferenceClient, volumes map[string][]string, spec swarm.TaskSpec) error {
	if spec.ContainerSpec == nil {
		return nil
	}

	r.addLabels(spec.ContainerSpec.Labels)

	for _, m := range spec.ContainerSpec.Mounts {
		switch m.Type {
		case mount.TypeBind:
			r.addPath(m.Source)
		case mount.TypeVolume:
			if m.VolumeOptions != nil && m.VolumeOptions.DriverConfig != nil {
				// Nodes create the volume with these options where a task needs it.
				driver := m.VolumeOptions.DriverConfig
				if driver.Name == "" || driver.Name == "local" {
					device, err := localVolumeBindSource(m.Source, driver.Options)
					if err != nil {
						return err
					}

					r.addPath(device)
				}

				continue
			}

			// A named volume that already exists on this node is used as is.
			if err := r.addVolume(ctx, apiClient, volumes, m.Source); err != nil {
				return err
			}
		}
	}

	return nil
}

// addVolume adds the host paths that the volume called name on this node is stored in or binds.
func (r *sourceReferences) addVolume(ctx context.Context, apiClient sourceReferenceClient, volumes map[string][]string, name string) error {
	if name == "" {
		return nil
	}

	paths, ok := volumes[name]
	if !ok {
		result, err := apiClient.VolumeInspect(ctx, name, client.VolumeInspectOptions{})

		switch {
		case errdefs.IsNotFound(err):
			// A volume that does not exist holds no data.
		case err != nil:
			return fmt.Errorf("inspect volume %s: %w", name, err)
		default:
			paths = append(paths, result.Volume.Mountpoint)

			if result.Volume.Driver == "local" {
				device, err := localVolumeBindSource(name, result.Volume.Options)
				if err != nil {
					return err
				}

				paths = append(paths, device)
			}
		}

		volumes[name] = paths
	}

	for _, path := range paths {
		r.addPath(path)
	}

	return nil
}

// localVolumeBindSource returns the host directory a volume of the local driver with options binds, or "" if it does
// not bind one.
func localVolumeBindSource(name string, options map[string]string) (string, error) {
	if !slices.ContainsFunc(strings.Split(options["o"], ","), func(option string) bool {
		option = strings.TrimSpace(option)
		return option == "bind" || option == "rbind"
	}) {
		return "", nil
	}

	device := strings.TrimSpace(options["device"])
	if !filepath.IsAbs(device) {
		return "", fmt.Errorf("volume %s binds %q, which is not an absolute path", name, device)
	}

	return device, nil
}

// pathLabels are the labels recording directories and files that a container, service or task uses.
var pathLabels = []string{
	docker.DocoCDLabels.Deployment.WorkingDir,
	docker.DocoCDLabels.Source.ConfigWorkingDir,
	api.WorkingDirLabel,
}

func (r *sourceReferences) addLabels(labels map[string]string) {
	if len(labels) == 0 {
		return
	}

	for _, key := range pathLabels {
		r.addPath(labels[key])
	}

	for _, file := range strings.Split(labels[api.ConfigFilesLabel], ",") {
		r.addPath(file)
	}

	if repository := repositoryFromSourceLabels(labels); repository != "" {
		r.repositories.Add(repository)
	}

	if repository := docker.NormalizeRepositoryLabel(labels[docker.DocoCDLabels.Source.Name]); repository != "" {
		r.repositories.Add(repository)
	}
}

// addPath adds path if it is below the data directory.
func (r *sourceReferences) addPath(path string) {
	path = strings.TrimSpace(path)
	if !filepath.IsAbs(path) {
		return
	}

	for _, root := range r.roots {
		rel, err := filepath.Rel(root, filepath.Clean(path))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}

		// The data directory itself references nothing in particular.
		if rel != "." {
			r.paths.Add(filepath.ToSlash(rel))
		}

		return
	}
}

// referencedBy returns what references the store whose path relative to the data directory is storeRel, or "" if
// nothing does.
func (r *sourceReferences) referencedBy(storeRel string) string {
	storeRel = filepath.ToSlash(storeRel)

	for _, path := range set.SortedSlice(r.paths) {
		if path == storeRel || strings.HasPrefix(path, storeRel+"/") || strings.HasPrefix(storeRel, path+"/") {
			return "path " + path
		}
	}

	for _, repository := range set.SortedSlice(r.repositories) {
		if repositoryKeyMatches(storeRel, repository) {
			return "source " + repository
		}
	}

	return ""
}
