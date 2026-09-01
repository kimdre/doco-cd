package docker

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"

	"github.com/kimdre/doco-cd/internal/common/validation"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/prometheus"
	"github.com/kimdre/doco-cd/internal/webhook"
)

type deploymentPhaseState struct {
	mu    sync.RWMutex
	phase string
}

func newDeploymentPhaseState(initialPhase string) *deploymentPhaseState {
	return &deploymentPhaseState{phase: normalizeDeploymentPhase(initialPhase)}
}

func (s *deploymentPhaseState) Set(phase string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.phase = normalizeDeploymentPhase(phase)
}

func (s *deploymentPhaseState) Get() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.phase
}

func normalizeDeploymentPhase(phase string) string {
	phase = strings.TrimSpace(phase)
	if phase == "" {
		return "unknown"
	}

	return phase
}

func setDeploymentPhase(setPhase func(string), phase string) {
	if setPhase != nil {
		setPhase(phase)
	}
}

func logDeploymentHeartbeat(log *slog.Logger, phase string) {
	log.Info("deployment in progress", slog.String("phase", normalizeDeploymentPhase(phase)))
}

func deploymentRepositoryKey(payload *webhook.ParsedPayload) string {
	if payload == nil {
		return ""
	}

	for _, candidate := range []string{payload.CloneURL, payload.FullName, payload.Artifact} {
		if value := strings.TrimSpace(candidate); value != "" {
			return value
		}
	}

	return ""
}

func resolveDeploymentMetricsRepositoryLabel(payload *webhook.ParsedPayload) string {
	repository := normalizeRepositoryForLabelMatch(deploymentRepositoryKey(payload))
	if repository == "" {
		return "unknown"
	}

	return repository
}

func resolveDeploymentMetricsDeploymentLabel(deployName string) string {
	deployment := strings.TrimSpace(deployName)
	if deployment == "" {
		return "unknown"
	}

	return deployment
}

// DeployRequest bundles DeployStack's per-deployment input.
type DeployRequest struct {
	JobLog           *slog.Logger `validate:"required,nostructlevel"`
	ExternalRepoPath string       `validate:"required"`
	DockerCLI        command.Cli  `validate:"required,nostructlevel"`
	Payload          *webhook.ParsedPayload
	DeployConfig     *deploy.Config `validate:"required,nostructlevel"`
	DetectedChanges  []Change
	NeedSignal       []SignalService
	LatestCommit     string
	AppVersion       string `validate:"required"`
	ComposeLoad      ComposeLoadOptions
	SwarmRetention   SwarmRetentionOptions
	SwarmMode        bool
	HashNormMap      map[string]string
}

type runtimeDeployRequest struct {
	request            DeployRequest
	project            *types.Project
	externalWorkingDir string
	timestamp          string
	projectHash        string
	phase              *deploymentPhaseState
	stackLog           *slog.Logger
	repositoryLabel    string
	deploymentLabel    string
	contextLabel       string
}

func (r runtimeDeployRequest) recordError() {
	prometheus.DeploymentErrorsTotal.WithLabelValues(r.repositoryLabel, r.deploymentLabel, r.contextLabel).Inc()
}

// DeployStack performs shared deployment lifecycle work and dispatches to the selected runtime.
func DeployStack(ctx context.Context, req DeployRequest) error {
	if err := validation.Validate(req); err != nil {
		return fmt.Errorf("validate deploy stack request: %w", err)
	}

	startTime := time.Now()
	repositoryLabel := resolveDeploymentMetricsRepositoryLabel(req.Payload)
	deploymentLabel := resolveDeploymentMetricsDeploymentLabel(req.DeployConfig.Name)
	contextLabel := DisplayContextName(req.DeployConfig.Context)
	stackLog := req.JobLog.With(slog.String("stack", req.DeployConfig.Name))

	stackLog.Debug("waiting for scheduler/deploy lock")

	stackLockKey := lock.StackKey(req.DeployConfig.Context, req.DeployConfig.Name)

	lock.LockStack(stackLockKey)
	defer lock.UnlockStack(stackLockKey)

	stackLog.Debug("acquired scheduler/deploy lock")

	deploymentPhase := newDeploymentPhaseState("resolving working directory")
	externalWorkingDir := path.Join(req.ExternalRepoPath, req.DeployConfig.WorkingDirectory)

	externalWorkingDir, err := filepath.Abs(externalWorkingDir)
	if err != nil || !strings.HasPrefix(externalWorkingDir, req.ExternalRepoPath) {
		errMsg := "invalid working directory: resolved path is outside the allowed base directory"
		req.JobLog.Error(errMsg, slog.String("resolved_path", externalWorkingDir))

		return fmt.Errorf("%s", errMsg)
	}

	deploymentPhase.Set("loading compose configuration")

	project, err := LoadCompose(
		ctx,
		req.DockerCLI,
		req.ExternalRepoPath,
		externalWorkingDir,
		req.DeployConfig.Name,
		req.DeployConfig.ComposeFiles,
		req.DeployConfig.EnvFiles,
		req.DeployConfig.Profiles,
		req.DeployConfig.Internal.Environment,
		req.ComposeLoad,
	)
	if err != nil {
		return fmt.Errorf("failed to load compose config: %w", err)
	}

	if err = validateScheduledJobPolicies(project, req.SwarmMode); err != nil {
		return fmt.Errorf("invalid scheduled job restart policy: %w", err)
	}

	if req.DeployConfig.WaitRunningJobs {
		deploymentPhase.Set("waiting for running scheduled jobs")

		if err = waitForRunningJobs(ctx, req.DockerCLI, req.DeployConfig, project, stackLog, req.SwarmMode); err != nil {
			return err
		}
	}

	done := make(chan struct{})
	defer close(done)

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				logDeploymentHeartbeat(stackLog, deploymentPhase.Get())
			case <-done:
				return
			}
		}
	}()

	timestamp := time.Now().UTC().Format(time.RFC3339)

	projectHash, err := ProjectHash(WithNormalizedEnvValues(project, req.HashNormMap))
	if err != nil {
		return fmt.Errorf("failed to generate project hash: %w", err)
	}

	runtimeReq := runtimeDeployRequest{
		request:            req,
		project:            project,
		externalWorkingDir: externalWorkingDir,
		timestamp:          timestamp,
		projectHash:        projectHash,
		phase:              deploymentPhase,
		stackLog:           stackLog,
		repositoryLabel:    repositoryLabel,
		deploymentLabel:    deploymentLabel,
		contextLabel:       contextLabel,
	}

	if req.SwarmMode {
		err = deploySwarmRuntime(ctx, runtimeReq)
	} else {
		err = deployComposeRuntime(ctx, runtimeReq)
	}

	if err != nil {
		return err
	}

	deploymentPhase.Set("finalizing deployment status")

	setDeployStatusToCache(gitInternal.GetRepoName(deploymentRepositoryKey(req.Payload)), req.DeployConfig.Name,
		deployStatus{
			CommitSHA:   req.LatestCommit,
			ComposeHash: projectHash,
		},
	)

	prometheus.DeploymentsTotal.WithLabelValues(repositoryLabel, deploymentLabel, contextLabel).Inc()
	prometheus.DeploymentDuration.WithLabelValues(repositoryLabel, deploymentLabel, contextLabel).Observe(time.Since(startTime).Seconds())

	return nil
}
