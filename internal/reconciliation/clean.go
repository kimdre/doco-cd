package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/docker/cli/cli/command"

	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/config"
	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/source/oci"
	"github.com/kimdre/doco-cd/internal/source/store"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// cleanupObsoleteAutoDiscoveredContainers removes auto-discovered stacks of req's source that are not part of
// deployConfigs anymore.
//
// The removal policy of a stack comes from the auto-discovery configs in deployConfigs that scan its directory.
// The policy recorded in the stack's labels is only used if none of them does, e.g. because the auto-discovery
// config was removed. A stack is kept if the webhook event filter of such a config does not match the event, or
// if it runs a newer revision than the one its directory was scanned in.
//
// contextName is the Docker context dockerCli is connected to. allowRemoval, if not nil, decides whether an
// obsolete stack may be removed now (see syncWindowGate.allowRemoval); stacks it rejects are kept.
func cleanupObsoleteAutoDiscoveredContainers(ctx context.Context, jobLog *slog.Logger,
	dockerCli command.Cli, swarmMode bool, contextName string,
	req DeployRequest, deployConfigs []*deployConfig.Config,
	notifier notification.Sender, allowRemoval func(stackLog *slog.Logger, stackName string) bool,
) error {
	c := newObsoleteStackCleanup(jobLog, dockerCli, swarmMode, contextName, req, deployConfigs)

	serviceLabels, err := docker.GetAutoDiscoveryServices(ctx, dockerCli.Client(), swarmMode)
	if err != nil {
		if serviceLabels == nil {
			return fmt.Errorf("failed to retrieve containers for auto-discovery cleanup: %w", err)
		}

		c.log.Warn("failed to migrate auto-discovery labels for some services; continuing cleanup",
			logger.ErrAttr(err),
		)
	}

	stacks := make(map[string]map[docker.Service]map[string]string)

	for service, labels := range serviceLabels {
		stackName := labels[docker.DocoCDLabels.Deployment.Name]
		if stackName == "" {
			continue
		}

		if stacks[stackName] == nil {
			stacks[stackName] = make(map[docker.Service]map[string]string)
		}

		stacks[stackName][service] = labels
	}

	var errs []error

	for _, stackName := range slices.Sorted(maps.Keys(stacks)) {
		services := stacks[stackName]
		stackLog := c.log.With(slog.String("stack", stackName))

		if c.present.Contains(stackName) {
			stackLog.Debug("auto-discovered stack is present in current config, skipping obsolete cleanup")

			continue
		}

		policy, remove := c.removalPolicy(stackLog, services)
		if !remove {
			continue
		}

		if allowRemoval != nil && !allowRemoval(stackLog, stackName) {
			continue
		}

		if err := c.remove(ctx, stackLog, stackName, services, policy, notifier); err != nil {
			errs = append(errs, fmt.Errorf("failed to remove obsolete auto-discovered stack '%s': %w", stackName, err))
		}
	}

	return errors.Join(errs...)
}

// obsoleteStackCleanup holds the state of one cleanup run.
type obsoleteStackCleanup struct {
	log         *slog.Logger
	dockerCli   command.Cli
	swarmMode   bool
	contextName string
	req         DeployRequest
	// present holds the names of all deploy configs, discovered or not.
	present set.Set[string]
	targets set.Set[string]
	// origins are the auto-discovery configs that discovered deploy configs.
	origins  []deployConfig.AutoDiscoveryOrigin
	ancestry *stages.GitAncestryCache
}

func newObsoleteStackCleanup(jobLog *slog.Logger, dockerCli command.Cli, swarmMode bool, contextName string,
	req DeployRequest, deployConfigs []*deployConfig.Config,
) *obsoleteStackCleanup {
	c := &obsoleteStackCleanup{
		log:         jobLog.With(slog.String("repo_clone_url", req.Repository.SourceUrl)),
		dockerCli:   dockerCli,
		swarmMode:   swarmMode,
		contextName: contextName,
		req:         req,
		present:     set.New[string](),
		targets:     set.New[string](),
		ancestry:    stages.NewGitAncestryCache(),
	}

	seen := set.New[deployConfig.AutoDiscoveryOrigin]()

	for _, cfg := range deployConfigs {
		c.present.Add(cfg.Name)
		c.targets.Add(strings.TrimSpace(cfg.Internal.ConfigTarget))

		if origin := cfg.Internal.AutoDiscoveryOrigin; origin != nil && !seen.Contains(*origin) {
			seen.Add(*origin)
			c.origins = append(c.origins, *origin)
		}
	}

	return c
}

// removalPolicy decides whether the obsolete stack with services may be removed and how.
func (c *obsoleteStackCleanup) removalPolicy(stackLog *slog.Logger, services map[docker.Service]map[string]string) (deployConfig.AutoDiscoveryConfig, bool) {
	policy := deployConfig.AutoDiscoveryConfig{Delete: true, RemoveVolumes: true, RemoveImages: true}

	for _, service := range slices.Sorted(maps.Keys(services)) {
		servicePolicy, remove := c.servicePolicy(stackLog, services[service])
		if !remove {
			return deployConfig.AutoDiscoveryConfig{}, false
		}

		policy.RemoveVolumes = policy.RemoveVolumes && servicePolicy.RemoveVolumes
		policy.RemoveImages = policy.RemoveImages && servicePolicy.RemoveImages
	}

	return policy, len(services) > 0
}

// servicePolicy decides whether the service of an obsolete stack with labels may be removed and how.
func (c *obsoleteStackCleanup) servicePolicy(stackLog *slog.Logger, labels map[string]string) (deployConfig.AutoDiscoveryConfig, bool) {
	// Filter out stacks belonging to a different deployment config target before doing the
	// more expensive repository URL comparison (and its associated logging). This avoids
	// needlessly scanning/logging stacks that were auto-discovered by other doco-cd targets
	// sharing the same Docker host and repository (e.g., a "nas" target's auto-discovered
	// stacks while reconciling for an "updater" target that doesn't use auto-discovery at all).
	stackConfigTarget := strings.TrimSpace(labels[docker.DocoCDLabels.Deployment.ConfigTarget])
	if !isCleanupTargetMatch(c.targets, stackConfigTarget) {
		stackLog.Debug("skipping auto-discovered stack as it belongs to a different deployment config target",
			slog.String("stack_config_target", stackConfigTarget),
			slog.Any("run_config_targets", sortedTargetKeys(c.targets)),
		)

		return deployConfig.AutoDiscoveryConfig{}, false
	}

	// The URLs may differ in format (e.g., "https://github.com/kimdre/doco-cd.git" vs.
	// "https://github.com/kimdre/doco-cd") or protocol (e.g., "ssh://git@github.com/kimdre/doco-cd.git"), and
	// OCI references in their digest. The label holds the URL of the job's source, see
	// stages.sourceURLForLabels. OCI storage identity is repository-based, but cleanup
	// ownership also requires the stable reference below.
	cloneURL := c.req.Repository.SourceUrl

	labelURL := labels[docker.DocoCDLabels.Source.URL]
	if c.req.Repository.Source == config.SourceTypeOCI &&
		(strings.TrimSpace(cloneURL) == "" || config.OciUrl(cloneURL).Validate() != nil ||
			strings.TrimSpace(labelURL) == "" || config.OciUrl(labelURL).Validate() != nil) {
		stackLog.Warn("skipping obsolete OCI stack with missing or invalid source URL")

		return deployConfig.AutoDiscoveryConfig{}, false
	}

	cloneURLRepoName := c.sourceRepoName(cloneURL)
	labelURLRepoName := c.sourceRepoName(labelURL)
	match := cloneURLRepoName == labelURLRepoName

	stackLog.Debug("checking auto-discovered stack for repository match",
		slog.Group("repo_url",
			slog.String("clone_url", cloneURL),
			slog.String("clone_url_repo_name", cloneURLRepoName),
			slog.String("label_url", labelURL),
			slog.String("label_url_repo_name", labelURLRepoName),
		),
		slog.Bool("match", match),
	)

	if !match {
		stackLog.Debug("skipping auto-discovered stack as it belongs to a different repository")

		return deployConfig.AutoDiscoveryConfig{}, false
	}

	if c.req.Repository.Source == config.SourceTypeOCI && !c.ociOwnerMatches(stackLog, labels) {
		return deployConfig.AutoDiscoveryConfig{}, false
	}

	deployed := labels[docker.DocoCDLabels.Deployment.CommitSHA]
	deployedReference := labels[docker.DocoCDLabels.Deployment.TargetRef]

	owners := c.owners(labels[docker.DocoCDLabels.Deployment.WorkingDir])
	if len(owners) == 0 {
		// No current auto-discovery config scans the stack's directory, fall back to the
		// policy the stack was deployed with.
		if stages.IsStaleRevision(c.req.Repository.MirrorDir, c.req.Repository.Revision, deployed, c.ancestry, stackLog) {
			return deployConfig.AutoDiscoveryConfig{}, false
		}

		policy := docker.ParseAutoDiscoveryConfig(labels[docker.DocoCDLabels.Deployment.AutoDiscoveryConfig])
		if !policy.Delete {
			stackLog.Debug("skipping removal of obsolete auto-discovered stack as per configuration")

			return deployConfig.AutoDiscoveryConfig{}, false
		}

		return policy, true
	}

	policy := deployConfig.AutoDiscoveryConfig{Delete: true, RemoveVolumes: true, RemoveImages: true}
	hasReferenceOwner := slices.ContainsFunc(owners, func(owner deployConfig.AutoDiscoveryOrigin) bool {
		return strings.TrimSpace(owner.Reference) != "" && strings.TrimSpace(deployedReference) != "" &&
			cleanupRevisionApplies(owner.Reference, deployedReference)
	})

	for _, owner := range owners {
		if !stages.WebhookEventFilterMatches(c.req.JobTrigger, owner.WebhookEventFilter, c.req.Payload) {
			stackLog.Debug("skipping obsolete auto-discovered stack as the webhook event filter of its auto-discovery config does not match",
				slog.String("webhook_filter", owner.WebhookEventFilter))

			return deployConfig.AutoDiscoveryConfig{}, false
		}

		if (!hasReferenceOwner || cleanupRevisionApplies(owner.Reference, deployedReference)) &&
			stages.IsStaleRevision(owner.MirrorDir, owner.Revision, deployed, c.ancestry, stackLog) {
			return deployConfig.AutoDiscoveryConfig{}, false
		}

		policy.Delete = policy.Delete && owner.Settings.Delete
		policy.RemoveVolumes = policy.RemoveVolumes && owner.Settings.RemoveVolumes
		policy.RemoveImages = policy.RemoveImages && owner.Settings.RemoveImages
	}

	if !policy.Delete {
		stackLog.Debug("skipping removal of obsolete auto-discovered stack as per configuration")

		return deployConfig.AutoDiscoveryConfig{}, false
	}

	return policy, true
}

// ociOwnerMatches separates independent tags sharing one source store. A digest-only source
// needs stable reference metadata; the immutable revision itself is not a lifecycle owner.
func (c *obsoleteStackCleanup) ociOwnerMatches(stackLog *slog.Logger, labels map[string]string) bool {
	runRef := c.req.Repository.ResolvedReference
	if runRef == "" && c.req.Payload != nil && c.req.Payload.Source == webhook.PayloadSourceOCI {
		runRef = c.req.Payload.Ref
	}

	stackRef := ""
	if _, inOCIStore := artifactRelativeDir(labels[docker.DocoCDLabels.Deployment.WorkingDir],
		c.sourceRepoName(labels[docker.DocoCDLabels.Source.URL])); inOCIStore &&
		labels[docker.DocoCDLabels.Source.Type] == string(config.SourceTypeOCI) {
		// repository_url deployments record a Git deployment reference instead. Only
		// use TargetRef when the deployment itself lives in the OCI source store.
		stackRef = labels[docker.DocoCDLabels.Deployment.TargetRef]
	}

	runOwner := ociCleanupReference(c.req.Repository.SourceUrl, runRef)

	stackOwner := ociCleanupReference(labels[docker.DocoCDLabels.Source.URL], stackRef)
	if runOwner == "" || stackOwner == "" {
		stackLog.Warn("skipping obsolete OCI stack because its stable reference ownership is missing or ambiguous",
			slog.String("run_reference", runOwner), slog.String("stack_reference", stackOwner))

		return false
	}

	if runOwner != stackOwner {
		stackLog.Debug("skipping auto-discovered OCI stack belonging to a different source reference",
			slog.String("run_reference", runOwner), slog.String("stack_reference", stackOwner))

		return false
	}

	return true
}

func ociCleanupReference(artifact, reference string) string {
	artifact = strings.TrimSpace(artifact)
	reference = strings.TrimSpace(reference)

	if artifact == "" || config.OciUrl(artifact).Validate() != nil {
		return ""
	}

	identifier := oci.TagFromArtifact(artifact)
	if !strings.Contains(artifact, "@") {
		// The config source URL is authoritative; TargetRef can name a Git
		// deployment branch when repository_url is set.
		return identifier
	}

	// ExtractOciArtifactTag historically records a digest's bare hash in TargetRef.
	// Neither that hash nor the complete digest establishes a stable tag owner.
	_, hash, _ := strings.Cut(identifier, ":")
	if reference == "" || reference == identifier || reference == hash {
		return ""
	}

	tagged := oci.RepositoryNameFromArtifact(artifact) + ":" + reference
	if config.OciUrl(tagged).Validate() != nil || oci.TagFromArtifact(tagged) != reference {
		return ""
	}

	return reference
}

// Missing reference metadata retains the conservative ancestry guard used by
// older deployments. Known, different references do not establish staleness.
func cleanupRevisionApplies(reference, deployedReference string) bool {
	reference = strings.TrimSpace(reference)
	deployedReference = strings.TrimSpace(deployedReference)

	return reference == "" || deployedReference == "" ||
		git.ReferenceMatches(reference, deployedReference) || git.ReferenceMatches(deployedReference, reference)
}

// owners returns the auto-discovery configs that scan workingDir, the working directory of a stack.
func (c *obsoleteStackCleanup) owners(workingDir string) []deployConfig.AutoDiscoveryOrigin {
	var owners []deployConfig.AutoDiscoveryOrigin

	for _, origin := range c.origins {
		relDir, ok := artifactRelativeDir(workingDir, c.storeName(origin))
		if ok && origin.Owns(relDir) {
			owners = append(owners, origin)
		}
	}

	return owners
}

// storeName returns the directory name of the source store origin scans, see source.Prepare.
func (c *obsoleteStackCleanup) storeName(origin deployConfig.AutoDiscoveryOrigin) string {
	if origin.RepositoryURL != "" {
		return git.GetRepoName(origin.RepositoryURL)
	}

	return c.sourceRepoName(c.req.Repository.SourceUrl)
}

// sourceRepoName returns the repository name of url, a URL of the job's source, without the tag or digest
// of an OCI artifact reference.
func (c *obsoleteStackCleanup) sourceRepoName(url string) string {
	if c.req.Repository.Source == config.SourceTypeOCI {
		return oci.RepositoryNameFromArtifact(url)
	}

	return git.GetRepoName(url)
}

// artifactRelativeDir returns workingDir relative to the root of the artifact it lies in, if that artifact
// belongs to the source store named storeName ("<data>/<storeName>/artifacts/<revision>/...").
func artifactRelativeDir(workingDir, storeName string) (string, bool) {
	if workingDir == "" || storeName == "" {
		return "", false
	}

	dir := filepath.ToSlash(filepath.Clean(workingDir)) + "/"

	_, rest, found := strings.Cut(dir, "/"+storeName+"/"+store.ArtifactsSubdir+"/")
	if !found {
		return "", false
	}

	revision, relDir, _ := strings.Cut(strings.TrimSuffix(rest, "/"), "/")
	if revision == "" {
		return "", false
	}

	if relDir == "" {
		return ".", true
	}

	return relDir, true
}

// remove destroys the obsolete stack with services. It holds the stack's deployment lock and skips the stack if
// it was redeployed or removed since services were listed.
func (c *obsoleteStackCleanup) remove(ctx context.Context, stackLog *slog.Logger, stackName string,
	services map[docker.Service]map[string]string, policy deployConfig.AutoDiscoveryConfig, notifier notification.Sender,
) error {
	stackLockKey := lock.StackKey(c.contextName, stackName)

	lock.LockStack(stackLockKey)
	defer lock.UnlockStack(stackLockKey)

	current, err := docker.GetDeploymentServices(ctx, c.dockerCli.Client(), c.swarmMode, stackName)
	if err != nil {
		return fmt.Errorf("failed to re-read stack: %w", err)
	}

	if len(current) == 0 {
		stackLog.Debug("obsolete auto-discovered stack is already removed")

		return nil
	}

	if deploymentFingerprint(current) != deploymentFingerprint(services) {
		stackLog.Info("skipping removal of obsolete auto-discovered stack as it was redeployed during cleanup")

		return nil
	}

	stackLog.Info("removing obsolete auto-discovered stack")

	removeConfig := &deployConfig.Config{Name: stackName}
	removeConfig.Destroy.Enabled = true
	removeConfig.Destroy.RemoveVolumes = policy.RemoveVolumes
	removeConfig.Destroy.RemoveImages = policy.RemoveImages

	dockerCli := c.dockerCli
	if err := docker.DestroyStack(c.log, &ctx, &dockerCli, removeConfig, c.swarmMode); err != nil {
		return err
	}

	stackLog.Info("removed obsolete auto-discovered stack")

	if notifier == nil {
		return nil
	}

	var stackConfigTarget string
	for _, labels := range services {
		stackConfigTarget = strings.TrimSpace(labels[docker.DocoCDLabels.Deployment.ConfigTarget])
	}

	notifyMetadata := c.req.Metadata
	notifyMetadata.Target = stackConfigTarget
	notifyMetadata.Stack = stackName
	notifyMetadata.Context = c.contextName

	if err := notifier.Send(notification.Success, "Stack destroyed", "successfully destroyed stack "+stackName, notifyMetadata); err != nil {
		stackLog.Error("failed to send notification", logger.ErrAttr(err))
	}

	return nil
}

// deploymentFingerprint identifies the deployment of a stack's services, so a redeployment changes it.
func deploymentFingerprint(services map[docker.Service]map[string]string) string {
	entries := make([]string, 0, len(services))

	for service, labels := range services {
		entries = append(entries, strings.Join([]string{
			string(service),
			labels[docker.DocoCDLabels.Deployment.Timestamp],
			labels[docker.DocoCDLabels.Deployment.CommitSHA],
			labels[docker.DocoCDLabels.Deployment.ConfigHash],
		}, "\x00"))
	}

	slices.Sort(entries)

	return strings.Join(entries, "\n")
}

// isCleanupTargetMatch checks if the stack's config target matches any of the run config targets.
func isCleanupTargetMatch(runConfigTargets set.Set[string], stackConfigTarget string) bool {
	// Backward compatibility: if no run target context is available, keep legacy behavior.
	if runConfigTargets.IsEmpty() {
		return true
	}

	stackConfigTarget = strings.TrimSpace(stackConfigTarget)

	// Backward compatibility for pre-label deployments: only include unlabeled stacks
	// for default-target runs, never for custom targets.
	if stackConfigTarget == "" {
		return runConfigTargets.Contains("")
	}

	return runConfigTargets.Contains(stackConfigTarget)
}

func sortedTargetKeys(m set.Set[string]) []string {
	if m.IsEmpty() {
		return nil
	}

	keys := m.ToSlice()

	slices.Sort(keys)

	return keys
}
