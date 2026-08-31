package docker

import (
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/go-git/go-git/v5/plumbing/format/diff"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/common/types/slice"
	"github.com/kimdre/doco-cd/internal/filesystem"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
)

func GetPathsFromGitChangedFiles(changedFiles []gitInternal.ChangedFile, basePath string) []string {
	var absPaths []string

	basePath = filepath.Clean(basePath)

	for _, f := range changedFiles {
		checkPaths := []diff.File{f.From, f.To}

		for _, checkPath := range checkPaths {
			if checkPath == nil {
				continue
			}

			p := filepath.Clean(checkPath.Path())

			if !filepath.IsAbs(p) {
				p = filepath.Join(basePath, p)
			}

			absPaths = append(absPaths, p)
		}
	}

	return slice.Unique(absPaths)
}

// HasChangedConfigs checks if any files used in docker compose `configs:` definitions have changed using the Git status.
func HasChangedConfigs(repoPathExternal string, paths []string, project *types.Project, ignoreCfg projectIgnoreCfg) ([]string, []string) {
	configToServicesMap := make(map[string][]string)

	for name, s := range project.Services {
		for _, cfg := range s.Configs {
			configToServicesMap[cfg.Source] = append(configToServicesMap[cfg.Source], name)
		}
	}

	var (
		changedServices []string
		ignoredServices []string
	)

	for cfgName, c := range project.Configs {
		// Changes in config.Content are handled in project hash comparison
		if c.File == "" {
			continue
		}

		for _, p := range paths {
			// ignore change outside repo
			if filesystem.InBasePath(c.File, p) &&
				filesystem.InBasePath(repoPathExternal, c.File) {
				for _, svcName := range configToServicesMap[cfgName] {
					if !checkIsIgnoreByCfg(ignoreCfg, svcName, changeScopeConfigs, cfgName) {
						changedServices = append(changedServices, svcName)
					} else {
						ignoredServices = append(ignoredServices, svcName)
					}
				}
			}
		}
	}

	return getChangeAndIgnore(changedServices, ignoredServices)
}

// HasChangedSecrets checks if any files used in docker compose `secrets:` definitions have changed using the Git status.
func HasChangedSecrets(repoPathExternal string, paths []string, project *types.Project, ignoreCfg projectIgnoreCfg) ([]string, []string) {
	secretsToServicesMap := make(map[string][]string)

	for name, s := range project.Services {
		for _, secret := range s.Secrets {
			secretsToServicesMap[secret.Source] = append(secretsToServicesMap[secret.Source], name)
		}
	}

	var (
		changedServices []string
		ignoredServices []string
	)

	for secretName, s := range project.Secrets {
		if s.File == "" {
			continue
		}

		for _, p := range paths {
			// ignore change outside repo
			if filesystem.InBasePath(s.File, p) &&
				filesystem.InBasePath(repoPathExternal, s.File) {
				for _, svcName := range secretsToServicesMap[secretName] {
					if !checkIsIgnoreByCfg(ignoreCfg, svcName, changeScopeSecrets, secretName) {
						changedServices = append(changedServices, svcName)
					} else {
						ignoredServices = append(ignoredServices, svcName)
					}
				}
			}
		}
	}

	return getChangeAndIgnore(changedServices, ignoredServices)
}

// HasChangedBindMounts checks if any files used in docker compose `volumes:` definitions with type `bind` have changed using the Git status.
func HasChangedBindMounts(repoPathExternal string, paths []string, project *types.Project, ignoreCfg projectIgnoreCfg) ([]string, []string) {
	var (
		changedServices []string
		ignoredServices []string
	)

	for _, s := range project.Services {
	out:
		for _, v := range s.Volumes {
			if v.Type == "bind" && v.Source != "" {
				for _, p := range paths {
					// ignore change outside repo
					if filesystem.InBasePath(v.Source, p) &&
						filesystem.InBasePath(repoPathExternal, v.Source) {
						if !checkIsIgnoreByCfg(ignoreCfg, s.Name, changeScopeBindMounts, v.Target) {
							changedServices = append(changedServices, s.Name)
						} else {
							ignoredServices = append(ignoredServices, s.Name)
						}

						break out
					}
				}
			}
		}
	}

	return getChangeAndIgnore(changedServices, ignoredServices)
}

// HasChangedEnvFiles checks if any files used in docker compose `env_file:` definitions have changed using the Git status.
func HasChangedEnvFiles(repoPathExternal string, paths []string, project *types.Project, _ projectIgnoreCfg) ([]string, []string) {
	var changedServices []string

	for _, s := range project.Services {
	out:
		for _, envFile := range s.EnvFiles {
			for _, p := range paths {
				// ignore change outside repo
				if filesystem.InBasePath(envFile.Path, p) &&
					filesystem.InBasePath(repoPathExternal, envFile.Path) {
					changedServices = append(changedServices, s.Name)
					break out
				}
			}
		}
	}

	return slice.Unique(changedServices), nil
}

// HasChangedBuildFiles checks if any files used as build context in docker compose `build:` definitions have changed using the Git status.
// This includes any file within the build context directory for each service. If a changed file is within a build context, it returns true.
func HasChangedBuildFiles(repoPathExternal string, paths []string, project *types.Project, _ projectIgnoreCfg) ([]string, []string) {
	var changedServices []string

	for _, s := range project.Services {
		if s.Build == nil {
			continue
		}

		buildContext := s.Build.Context
		additionalContexts := s.Build.AdditionalContexts
		dockerFile := s.Build.Dockerfile
		buildSecrets := s.Build.Secrets

		if buildContext == "" && len(additionalContexts) == 0 && dockerFile == "" && len(buildSecrets) == 0 {
			continue
		}

		var contexts []string

		if buildContext != "" {
			contexts = append(contexts, buildContext)
		}

		for _, v := range additionalContexts {
			if v != "" {
				contexts = append(contexts, v)
			}
		}

		for _, secret := range buildSecrets {
			if secret.Source != "" {
				contexts = append(contexts, secret.Source)
			}
		}

		if dockerFile != "" {
			contexts = append(contexts, dockerFile)
		}

	out:

		for _, ctxFile := range contexts {
			if !path.IsAbs(ctxFile) {
				ctxFile = filepath.Join(project.WorkingDir, ctxFile)
			}

			for _, p := range paths {
				// ignore change outside repo
				if filesystem.InBasePath(ctxFile, p) &&
					filesystem.InBasePath(repoPathExternal, ctxFile) {
					changedServices = append(changedServices, s.Name)
					break out
				}
			}
		}
	}

	return slice.Unique(changedServices), nil
}

type Change struct {
	Type     string
	Services []string
}

// forcedRecreateServices returns the services to force-recreate for the
// detected changes. A change without service scope (e.g. a failed-deploy
// retry) widens the force to the whole project, expressed as an empty set:
// compose selects all services when the list is empty.
func forcedRecreateServices(detectedChanges []Change) set.Set[string] {
	forced := set.New[string]()

	for _, change := range detectedChanges {
		if len(change.Services) == 0 {
			return set.New[string]()
		}

		forced.Add(change.Services...)
	}

	return forced
}

// sortChanges sorts the changes first by type and then by service name within each change.
func sortChanges(changes []Change) {
	slices.SortFunc(changes, func(a, b Change) int {
		return strings.Compare(a.Type, b.Type)
	})

	for i := range changes {
		slices.Sort(changes[i].Services)
	}
}

type IgnoredInfo struct {
	// Ignored services name
	Ignored []string `json:"ignored"`
	// Ignored services need to send signal
	NeedSendSignal []SignalService `json:"need_signal"`
}

func (i IgnoredInfo) IsEmpty() bool {
	return len(i.Ignored) == 0 && len(i.NeedSendSignal) == 0
}

func (i IgnoredInfo) IsNeedSignal() bool {
	return len(i.NeedSendSignal) > 0
}

type SignalService struct {
	ServiceName string `json:"service_name"`
	Signal      string `json:"signal"`
}

// ProjectFilesHaveChanges checks if any files related to the compose project have changed.
func ProjectFilesHaveChanges(repoPathExternal string, changePaths []string, project *types.Project) ([]Change, IgnoredInfo, error) {
	checks := []struct {
		name changeScope
		fn   func(string, []string, *types.Project, projectIgnoreCfg) ([]string, []string)
	}{
		{changeScopeConfigs, HasChangedConfigs},
		{changeScopeSecrets, HasChangedSecrets},
		{changeScopeBindMounts, HasChangedBindMounts},
		{changeScopeEnvFiles, HasChangedEnvFiles},
		{changeScopeBuildFiles, HasChangedBuildFiles},
	}

	ignoreCfg, err := getIgnoreRecreateCfgFromProject(project)
	if err != nil {
		return nil, IgnoredInfo{}, err
	}

	var (
		changes                                []Change
		allChangedServices, allIgnoredServices []string
	)

	for _, check := range checks {
		changedServices, ignoredServices := check.fn(repoPathExternal, changePaths, project, ignoreCfg)

		allChangedServices = append(allChangedServices, changedServices...)
		allIgnoredServices = append(allIgnoredServices, ignoredServices...)

		if len(changedServices) > 0 {
			slices.Sort(changedServices)

			changes = append(changes, Change{
				Type:     string(check.name),
				Services: changedServices,
			})
		}
	}

	sortChanges(changes)

	_, ignores := getChangeAndIgnore(allChangedServices, allIgnoredServices)
	slices.Sort(ignores)

	retIgnored := IgnoredInfo{}

	for _, svcName := range ignores {
		sig := ignoreCfg[svcName].signal
		if sig != "" {
			retIgnored.NeedSendSignal = append(retIgnored.NeedSendSignal, SignalService{
				ServiceName: svcName,
				Signal:      sig,
			})
		} else {
			retIgnored.Ignored = append(retIgnored.Ignored, svcName)
		}
	}

	return changes, retIgnored, nil
}
