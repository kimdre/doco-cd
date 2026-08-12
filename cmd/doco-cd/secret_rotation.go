package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/id"
	"github.com/kimdre/doco-cd/internal/config"
	appconfig "github.com/kimdre/doco-cd/internal/config/app"
	deployconfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/graceful"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
	"github.com/kimdre/doco-cd/internal/source/oci"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/webhook"
)

const secretRotationLoopTick = 10 * time.Second

type secretRotationTarget struct {
	key                string
	deploymentName     string
	sourceType         config.SourceType
	sourceURL          string
	sourceName         string
	sourceRef          string
	targetRef          string
	dockerContext      string
	configTarget       string
	encodedExternalRef map[string]string
	deployedHash       string
	interval           time.Duration
	rotateBefore       time.Duration
}

func StartSecretRotation(ctx context.Context, h *handlerData, wg *sync.WaitGroup) {
	if h == nil || h.appConfig == nil || h.log == nil || h.secretProvider == nil || *h.secretProvider == nil {
		return
	}

	graceful.SafeGo(wg, h.log.Logger, func() {
		runSecretRotationLoop(ctx, h)
	})
}

func runSecretRotationLoop(ctx context.Context, h *handlerData) {
	log := h.log.With(slog.String("component", "secret_rotation"))

	defaultInterval := effectiveSecretRotationInterval(h.appConfig)
	log.Info("starting secret rotation monitor", slog.String("default_interval", defaultInterval.String()))

	dockerClients := buildSecretRotationDockerClients(h, log)
	defer func() {
		for _, target := range dockerClients {
			if target.close != nil {
				target.close()
			}
		}
	}()

	ticker := time.NewTicker(secretRotationLoopTick)
	defer ticker.Stop()

	lastCheckedAt := map[string]time.Time{}
	lastResolvedHash := map[string]string{}
	inProgress := map[string]bool{}

	var mu sync.Mutex

	for {
		select {
		case <-ctx.Done():
			log.Info("secret rotation monitor stopped")
			return
		case <-ticker.C:
		}

		targetsByKey := map[string]secretRotationTarget{}

		for _, dockerTarget := range dockerClients {
			discovered, err := discoverSecretRotationTargets(ctx, dockerTarget.client, dockerTarget.contextName)
			if err != nil {
				log.Warn("failed to discover secret-rotation targets",
					slog.String("docker_context", dockerTarget.contextName),
					slog.String("error", err.Error()))

				continue
			}

			for _, target := range discovered {
				targetsByKey[target.key] = target
			}
		}

		now := time.Now()

		for _, t := range targetsByKey {
			interval := t.interval
			if interval <= 0 {
				interval = defaultInterval
			}

			if interval < deployconfig.MinSecretRotationInterval {
				interval = deployconfig.MinSecretRotationInterval
			}

			lastCheck := lastCheckedAt[t.key]
			if !lastCheck.IsZero() && now.Sub(lastCheck) < interval {
				continue
			}

			lastCheckedAt[t.key] = now

			shouldRotateByHint := false
			hintReason := ""

			if t.rotateBefore > 0 {
				var hintErr error

				shouldRotateByHint, hintReason, hintErr = shouldRotateByProviderHint(ctx, h.secretProvider, t)
				if hintErr != nil {
					log.Warn("failed provider-specific proactive hint check",
						slog.String("deployment", t.deploymentName),
						slog.String("source_url", t.sourceURL),
						slog.String("error", hintErr.Error()))
				}
			}

			// Resolve after the hint check. A hint provider may prepare replacement
			// values as part of its check; the redeploy only proceeds when those
			// values actually differ from the deployed hash.
			resolved, resolveErr := (*h.secretProvider).ResolveSecretReferences(ctx, t.encodedExternalRef)
			if resolveErr != nil {
				log.Warn("failed to resolve external secrets for rotation check",
					slog.String("deployment", t.deploymentName),
					slog.String("source_url", t.sourceURL),
					slog.String("error", resolveErr.Error()))

				continue
			}

			hash, hashErr := secrettypes.HashResolvedExternalSecretValuesFromMap(resolved)
			if hashErr != nil {
				log.Warn("failed to hash resolved external secrets",
					slog.String("deployment", t.deploymentName),
					slog.String("source_url", t.sourceURL),
					slog.String("error", hashErr.Error()))

				continue
			}

			changed := false

			switch {
			case strings.TrimSpace(t.deployedHash) != "":
				changed = hash != strings.TrimSpace(t.deployedHash)
			case lastResolvedHash[t.key] != "":
				changed = hash != lastResolvedHash[t.key]
			}

			lastResolvedHash[t.key] = hash

			if !changed {
				if shouldRotateByHint {
					log.Warn("provider requested proactive rotation but resolved values did not change; skipping redeploy",
						slog.String("deployment", t.deploymentName),
						slog.String("source_url", t.sourceURL),
						slog.String("reason", hintReason))
				}

				continue
			}

			mu.Lock()
			if inProgress[t.key] {
				mu.Unlock()
				continue
			}

			inProgress[t.key] = true
			mu.Unlock()

			go func(target secretRotationTarget, hinted bool, hintReason string) {
				defer func() {
					mu.Lock()
					delete(inProgress, target.key)
					mu.Unlock()
				}()

				cause := "external_secret_changed"
				if hinted {
					cause = "provider_hint"
				}

				if err := triggerSecretRotationDeployment(ctx, h, target, cause); err != nil {
					log.Error("secret-rotation deployment trigger failed",
						slog.String("deployment", target.deploymentName),
						slog.String("source_url", target.sourceURL),
						slog.String("error", err.Error()))

					return
				}

				log.Info("triggered deployment due to rotated external secrets",
					slog.String("deployment", target.deploymentName),
					slog.String("source_url", target.sourceURL),
					slog.String("cause", cause),
					slog.String("reason", hintReason))
			}(t, shouldRotateByHint, hintReason)
		}
	}
}

func shouldRotateByProviderHint(
	ctx context.Context,
	providerPtr *secretprovider.SecretProvider,
	target secretRotationTarget,
) (bool, string, error) {
	if providerPtr == nil || *providerPtr == nil || target.rotateBefore <= 0 {
		return false, "", nil
	}

	hints, ok := (*providerPtr).(secretprovider.SecretRotationHintProvider)
	if !ok {
		return false, "", nil
	}

	return hints.ShouldRotateSecretReferences(ctx, target.encodedExternalRef, target.rotateBefore)
}

func effectiveSecretRotationInterval(cfg *appconfig.Config) time.Duration {
	if cfg == nil || cfg.SecretRotationIntervalDefault <= 0 {
		return time.Hour
	}

	return cfg.SecretRotationIntervalDefault
}

type secretRotationDockerClient struct {
	contextName string
	client      client.APIClient
	close       func()
}

func buildSecretRotationDockerClients(h *handlerData, log *slog.Logger) []secretRotationDockerClient {
	result := []secretRotationDockerClient{{
		contextName: "",
		client:      h.dockerCli.Client(),
	}}

	contexts, err := h.dockerCli.ContextStore().List()
	if err != nil {
		log.Warn("failed to list Docker contexts for secret rotation", slog.String("error", err.Error()))
		return result
	}

	current := strings.TrimSpace(h.dockerCli.CurrentContext())
	for _, metadata := range contexts {
		name := strings.TrimSpace(metadata.Name)
		if name == "" || name == "default" || name == current {
			continue
		}

		contextCLI, err := docker.CreateDockerCliWithContext(h.appConfig.DockerQuietDeploy, name)
		if err != nil {
			log.Warn("failed to initialize Docker context for secret rotation",
				slog.String("docker_context", name),
				slog.String("error", err.Error()))

			continue
		}

		result = append(result, secretRotationDockerClient{
			contextName: name,
			client:      contextCLI.Client(),
			close: func() {
				_ = contextCLI.Client().Close()
			},
		})
	}

	return result
}

func discoverSecretRotationTargets(ctx context.Context, dockerClient client.APIClient, dockerContext string) ([]secretRotationTarget, error) {
	if dockerClient == nil {
		return nil, nil
	}

	result := map[string]secretRotationTarget{}

	services, svcErr := dockerClient.ServiceList(ctx, client.ServiceListOptions{})
	if svcErr == nil {
		for _, svc := range services.Items {
			t, ok := parseSecretRotationTargetFromLabels(svc.Spec.Labels, dockerContext)
			if !ok {
				continue
			}

			result[t.key] = t
		}
	}

	containers, err := dockerClient.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		if svcErr != nil {
			return nil, fmt.Errorf("failed to list services (%v) and containers (%w)", svcErr, err)
		}

		return nil, err
	}

	for _, c := range containers.Items {
		t, ok := parseSecretRotationTargetFromLabels(c.Labels, dockerContext)
		if !ok {
			continue
		}

		result[t.key] = t
	}

	out := make([]secretRotationTarget, 0, len(result))
	for _, t := range result {
		out = append(out, t)
	}

	return out, nil
}

func parseSecretRotationTargetFromLabels(labels map[string]string, discoveredContext string) (secretRotationTarget, bool) {
	if labels == nil {
		return secretRotationTarget{}, false
	}

	enabledRaw := strings.TrimSpace(labels[docker.DocoCDLabels.Deployment.SecretRotation])

	enabled, err := strconv.ParseBool(enabledRaw)
	if err != nil || !enabled {
		return secretRotationTarget{}, false
	}

	sourceURL := strings.TrimSpace(labels[docker.DocoCDLabels.Source.URL])
	sourceRef := strings.TrimSpace(labels[docker.DocoCDLabels.Source.Ref])
	targetRef := strings.TrimSpace(labels[docker.DocoCDLabels.Deployment.TargetRef])
	sourceType := config.NormalizeSourceType(config.SourceType(strings.TrimSpace(labels[docker.DocoCDLabels.Source.Type])))
	deploymentName := strings.TrimSpace(labels[docker.DocoCDLabels.Deployment.Name])

	externalRefsRaw := strings.TrimSpace(labels[docker.DocoCDLabels.Deployment.ExternalSecretsRefs])
	if sourceURL == "" || sourceRef == "" || targetRef == "" || externalRefsRaw == "" || deploymentName == "" {
		return secretRotationTarget{}, false
	}

	if err := config.ValidateSourceType(sourceType); err != nil {
		return secretRotationTarget{}, false
	}

	encodedRefs := map[string]string{}
	if err := json.Unmarshal([]byte(externalRefsRaw), &encodedRefs); err != nil || len(encodedRefs) == 0 {
		return secretRotationTarget{}, false
	}

	var interval time.Duration

	if raw := strings.TrimSpace(labels[docker.DocoCDLabels.Deployment.SecretRotationPeriod]); raw != "" {
		parsed, parseErr := time.ParseDuration(raw)
		if parseErr == nil {
			interval = parsed
		}
	}

	var rotateBefore time.Duration

	if raw := strings.TrimSpace(labels[docker.DocoCDLabels.Deployment.SecretRotateBefore]); raw != "" {
		parsed, parseErr := time.ParseDuration(raw)
		if parseErr == nil {
			rotateBefore = parsed
		}
	}

	configTarget := strings.TrimSpace(labels[docker.DocoCDLabels.Deployment.ConfigTarget])

	dockerContext := strings.TrimSpace(labels[docker.DocoCDLabels.Deployment.DockerContext])
	if dockerContext == "" {
		dockerContext = strings.TrimSpace(discoveredContext)
	}

	key := strings.Join([]string{
		string(sourceType),
		sourceURL,
		sourceRef,
		dockerContext,
		configTarget,
		deploymentName,
	}, "|")

	return secretRotationTarget{
		key:                key,
		deploymentName:     deploymentName,
		sourceType:         sourceType,
		sourceURL:          sourceURL,
		sourceName:         strings.TrimSpace(labels[docker.DocoCDLabels.Source.Name]),
		sourceRef:          sourceRef,
		targetRef:          targetRef,
		dockerContext:      dockerContext,
		configTarget:       configTarget,
		encodedExternalRef: encodedRefs,
		deployedHash:       strings.TrimSpace(labels[docker.DocoCDLabels.Deployment.ExternalSecretsHash]),
		interval:           interval,
		rotateBefore:       rotateBefore,
	}, true
}

func triggerSecretRotationDeployment(
	ctx context.Context,
	h *handlerData,
	t secretRotationTarget,
	cause string,
) error {
	jobID := id.GenID()

	metadata := notification.Metadata{
		Repository: git.GetRepoName(t.sourceURL),
		Target:     t.configTarget,
		Revision:   t.sourceRef,
		JobID:      jobID,
	}

	if t.sourceType == config.SourceTypeOCI {
		metadata.Repository = oci.RepositoryNameFromArtifact(t.sourceURL)
	}

	if h.runTracker != nil {
		h.runTracker.TrackAccepted(jobID, deploymentRunTriggerPoll)
		h.runTracker.SetMetadata(jobID, metadata.Repository, metadata.Target, metadata.Revision)
		h.runTracker.MarkRunning(jobID)
	}

	payload := webhook.ParsedPayload{
		Ref:      t.sourceRef,
		FullName: t.sourceName,
		WebURL:   t.sourceURL,
		Trigger:  "secret-rotation:" + cause,
	}

	if t.sourceType == config.SourceTypeOCI {
		payload.Source = webhook.PayloadSourceOCI
		payload.Artifact = t.sourceURL
	} else {
		payload.Source = webhook.PayloadSourceGit
	}

	pollCfg := poll.Config{
		Source:         t.sourceType,
		SourceUrl:      t.sourceURL,
		Reference:      t.sourceRef,
		CustomTarget:   t.configTarget,
		Deployments:    findSecretRotationInlineDeployments(h.appConfig, t),
		DeploymentName: t.deploymentName,
	}

	err := handle(
		ctx,
		h.log.With(slog.String("job_id", jobID), slog.String("trigger", "secret_rotation")),
		h.appConfig,
		h.dataMountPoint,
		h.secretProvider,
		h.dockerCli,
		stages.JobTriggerPoll,
		t.sourceType,
		t.sourceURL,
		t.sourceRef,
		false,
		metadata,
		t.configTarget,
		h.testName,
		pollCfg,
		payload,
	)

	if h.runTracker != nil {
		if err != nil {
			h.runTracker.MarkFailed(jobID, err.Error())
		} else {
			h.runTracker.MarkSucceeded(jobID, "secret rotation deployment completed")
		}
	}

	return err
}

func findSecretRotationInlineDeployments(appConfig *appconfig.Config, target secretRotationTarget) []*deployconfig.Config {
	if appConfig == nil {
		return nil
	}

	var autoDiscoveryFallback []*deployconfig.Config

	for i := range appConfig.PollConfig {
		pollConfig := &appConfig.PollConfig[i]
		if config.NormalizeSourceType(pollConfig.Source) != target.sourceType ||
			strings.TrimSpace(pollConfig.Reference) != target.sourceRef ||
			strings.TrimSpace(pollConfig.CustomTarget) != target.configTarget {
			continue
		}

		sourceURL := strings.TrimSpace(pollConfig.SourceUrl)
		if target.sourceType == config.SourceTypeGit {
			sourceURL, _ = rewriteSourceURL(sourceURL, appConfig.SourceURLRewrites)
		}

		if sourceURL != target.sourceURL {
			continue
		}

		for _, deployment := range pollConfig.Deployments {
			if deployment != nil && deployment.Name == target.deploymentName {
				return pollConfig.Deployments
			}

			if deployment != nil && deployment.AutoDiscovery.Enabled {
				autoDiscoveryFallback = pollConfig.Deployments
			}
		}
	}

	return autoDiscoveryFallback
}
