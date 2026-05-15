package docker

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"

	"github.com/kimdre/doco-cd/internal/utils/set"
	"github.com/kimdre/doco-cd/internal/utils/slice"
)

type changeScope string

const (
	changeScopeConfigs    changeScope = "configs"
	changeScopeSecrets    changeScope = "secrets"
	changeScopeBindMounts changeScope = "bindMounts"
	changeScopeBuildFiles changeScope = "buildFiles"
	changeScopeEnvFiles   changeScope = "envFiles"
	changeScopeSpec       changeScope = "spec"
)

type (
	// key is the service name.
	projectIgnoreCfg map[string]serviceIgnoreCfg

	serviceIgnoreCfg struct {
		ignoreMap ignoreCfg
		// send signal when ignore
		signal string
	}

	ignoreCfg map[changeScope]changeIgnoreRule
	// ignore specific items.
	// when null and empty, means ignore all.
	changeIgnoreRule []string
)

func (c changeIgnoreRule) IsIgnore(item string) bool {
	// empty items or null means ignore all
	if len(c) == 0 {
		return true
	}

	return slices.Contains(c, item)
}

var ErrIgnoreCfgInvalid = errors.New("ignore config is invalid")

// parseRecreateIgnore parses the recreate-ignore config
// example:  "{configs: [app, nginx], secrets: [db], bindMounts: []}"
func parseRecreateIgnore(input string) (ignoreCfg, error) {
	ret := ignoreCfg{}

	err := yaml.Unmarshal([]byte(input), &ret)
	if err != nil {
		return nil, fmt.Errorf("%w, yaml err: %v", ErrIgnoreCfgInvalid, err.Error())
	}

	for scope, rule := range ret {
		switch scope {
		case changeScopeConfigs, changeScopeSecrets, changeScopeBindMounts:
			// ignore envFiles and buildFiles because always need recreate
		case changeScopeSpec:
			for _, field := range rule {
				if !isSupportedServiceSpecField(field) {
					return nil, fmt.Errorf("%w, unsupported spec field %q", ErrIgnoreCfgInvalid, field)
				}
			}
		default:
			return nil, fmt.Errorf("%w, %s is not supported", ErrIgnoreCfgInvalid, scope)
		}

		if len(slice.Unique(rule)) != len(rule) {
			return nil, fmt.Errorf("%w, %s have duplicated items", ErrIgnoreCfgInvalid, scope)
		}
	}

	return ret, nil
}

// getIgnoreRecreateCfgFromProject returns the recreate-ignore config.
func getIgnoreRecreateCfgFromProject(project *types.Project) (projectIgnoreCfg, error) {
	ret := make(map[string]serviceIgnoreCfg)

	for name, s := range project.Services {
		rawIgnoreCfg, ignoreExist := s.Labels[DocoCDLabels.Deployment.RecreateIgnore]

		rawIgnoreCfg = strings.TrimSpace(rawIgnoreCfg)
		if ignoreExist && rawIgnoreCfg == "" {
			return nil, fmt.Errorf("service %s ignore is exist but empty, err: %w", name, ErrIgnoreCfgInvalid)
		}

		sig, sigExist := s.Labels[DocoCDLabels.Deployment.RecreateIgnoreSignal]

		sig = strings.TrimSpace(sig)
		if sigExist && sig == "" {
			return nil, fmt.Errorf("service %s ignore signal is exist but empty, err: %w", name, ErrIgnoreCfgInvalid)
		}

		externallyManaged, externallyManagedExist := s.Labels[DocoCDLabels.Service.ExternallyManaged]

		externallyManaged = strings.TrimSpace(externallyManaged)
		if externallyManagedExist {
			parsed, err := strconv.ParseBool(externallyManaged)
			if err != nil {
				return nil, fmt.Errorf("service %s externally_managed is invalid, err: %w", name, ErrIgnoreCfgInvalid)
			}

			if parsed {
				if ignoreExist {
					cfg, err := parseRecreateIgnore(rawIgnoreCfg)
					if err != nil {
						return nil, fmt.Errorf("%s's ignoreCfg is err: %w", name, err)
					}

					cfg[changeScopeSpec] = nil

					ret[name] = serviceIgnoreCfg{ignoreMap: cfg, signal: sig}
				} else {
					ret[name] = serviceIgnoreCfg{
						ignoreMap: ignoreCfg{changeScopeSpec: nil},
						signal:    sig,
					}
				}

				continue
			}
		}

		if ignoreExist {
			cfg, err := parseRecreateIgnore(rawIgnoreCfg)
			if err != nil {
				return nil, fmt.Errorf("%s's ignoreCfg is err: %w", name, err)
			}

			ret[name] = serviceIgnoreCfg{
				ignoreMap: cfg,
				signal:    sig,
			}
		} else if sigExist {
			return nil, fmt.Errorf("service %s, ignore signal is exist but ignore is missing err: %w", name, ErrIgnoreCfgInvalid)
		}
	}

	return ret, nil
}

func checkIsIgnoreByCfg(cfg projectIgnoreCfg, svc string, scope changeScope, item string) bool {
	svcCfg, ok := cfg[svc]
	if !ok {
		return false
	}

	scopeCfg, ok := svcCfg.ignoreMap[scope]
	if !ok {
		return false
	}

	return scopeCfg.IsIgnore(item)
}

func getChangeAndIgnore(changed, ignored []string) ([]string, []string) {
	changedSet := set.New(changed...)
	ignoredSet := set.New(ignored...)

	// if changed set contains ignored set, remove them
	ignoredSet = ignoredSet.Difference(changedSet)

	return changedSet.ToSlice(), ignoredSet.ToSlice()
}
