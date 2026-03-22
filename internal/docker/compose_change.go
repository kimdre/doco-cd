package docker

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

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
)

type changeIgnoreRule struct {
	Items []string // ignore specific items
}

func (c changeIgnoreRule) IsIgnore(item string) bool {
	// empty items means ignore all
	if len(c.Items) == 0 {
		return true
	}

	return slices.Contains(c.Items, item)
}

var (
	ErrChangeScopeDuplicate = errors.New("change scope is duplicated")
	ErrChangeScopeInvalid   = errors.New("change scope is invalid")
)

// parseRecreateIgnore parses the recreate-ignore config
// example: configs=app|nginx,secrets=db,bindMounts
func parseRecreateIgnore(input string) (map[changeScope]changeIgnoreRule, error) {
	ret := make(map[changeScope]changeIgnoreRule)

	input = strings.TrimSpace(input)
	if input == "" {
		return ret, nil
	}

	for entry := range strings.SplitSeq(input, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		scopeStr, itemsPart, _ := strings.Cut(entry, "=")
		scope := changeScope(scopeStr)

		switch scope {
		case changeScopeConfigs, changeScopeSecrets, changeScopeBindMounts:
			// ignore envFiles and buildFiles because always need recreate
		default:
			return nil, fmt.Errorf("%s: %w", scope, ErrChangeScopeInvalid)
		}
		// duplicate scope check
		_, ok := ret[scope]
		if ok {
			return nil, fmt.Errorf("%s: %w", scope, ErrChangeScopeDuplicate)
		}

		rule := changeIgnoreRule{}

		for item := range strings.SplitSeq(itemsPart, "|") {
			item = strings.TrimSpace(item)
			if item != "" {
				rule.Items = append(rule.Items, item)
			}
		}

		if len(rule.Items) != 0 {
			if len(slice.Unique(rule.Items)) != len(rule.Items) {
				return nil, fmt.Errorf("%s: %w", scope, ErrChangeScopeDuplicate)
			}
		}

		ret[scope] = rule
	}

	return ret, nil
}

// key is the service name.
type projectIgnoreCfg = map[string]serviceIgnoreCfg

type serviceIgnoreCfg struct {
	ignoreMap map[changeScope]changeIgnoreRule
	// send signal when ignore
	signal string
}

// getChangeFromProject returns the recreate-ignore config.
func getIgnoreRecrateCfgFromProject(project *types.Project) (projectIgnoreCfg, error) {
	ret := make(map[string]serviceIgnoreCfg)

	for name, s := range project.Services {
		ignoreCfg, ok := s.Labels[DocoCDLabels.Deployment.RecreateIgnore]
		if ok {
			cfg, err := parseRecreateIgnore(ignoreCfg)
			if err != nil {
				return nil, fmt.Errorf("%s's ignoreCfg is err: %w", name, err)
			}

			ret[name] = serviceIgnoreCfg{
				ignoreMap: cfg,
				signal:    s.Labels[DocoCDLabels.Deployment.RecreateIgnoreSignal],
			}
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
