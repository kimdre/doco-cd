package stages

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/syncwindow"
)

// ErrRetrySuppressed remains an intentional skip without implying success.
var ErrRetrySuppressed = fmt.Errorf("deployment retry suppressed: %w", ErrSkipDeployment)

// RetrySuppressedStack describes a single stack that was suppressed from retrying.
type RetrySuppressedStack struct {
	Name          string
	Context       string
	Revision      string
	Reason        string
	NextRetry     time.Time
	FailedStage   string
	FailureReason string
	RetriesUsed   int
	RetryLimit    *int
}

// RetrySuppressedError is returned when a deployment retry is suppressed due to retry delay or budget limits.
type RetrySuppressedError struct {
	Stacks []RetrySuppressedStack
}

// Error implements the error interface for RetrySuppressedError.
func (e *RetrySuppressedError) Error() string {
	descriptions := make([]string, 0, len(e.Stacks))
	for _, stack := range e.Stacks {
		description := stack.Name + " (" + docker.DisplayContextName(stack.Context) + "): " + stack.Reason
		if !stack.NextRetry.IsZero() {
			description += " until " + stack.NextRetry.Format(time.RFC3339)
		}

		descriptions = append(descriptions, description)
	}

	return "deployment retry suppressed for " + strings.Join(descriptions, ", ")
}

func (e *RetrySuppressedError) Unwrap() error {
	return ErrRetrySuppressed
}

// MergeRetrySuppressed merges multiple RetrySuppressedError instances into a single one.
func MergeRetrySuppressed(suppressed []*RetrySuppressedError) *RetrySuppressedError {
	merged := &RetrySuppressedError{}

	for _, err := range suppressed {
		if err != nil {
			merged.Stacks = append(merged.Stacks, err.Stacks...)
		}
	}

	if len(merged.Stacks) == 0 {
		return nil
	}

	slices.SortStableFunc(merged.Stacks, func(a, b RetrySuppressedStack) int {
		if order := strings.Compare(a.Context, b.Context); order != 0 {
			return order
		}

		return strings.Compare(a.Name, b.Name)
	})

	return merged
}

// retrySourceIdentity generates a hashed identity for a given source URL.
func retrySourceIdentity(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return ""
	}

	if parsed, err := url.Parse(source); err == nil {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.Fragment = ""
		source = parsed.String()
	}

	if kind, name := destroySourceIdentity(source); name != "" {
		source = string(kind) + ":" + name
	}

	return fmt.Sprintf("%x", sha256.Sum256([]byte(source)))
}

// deploymentFailureTarget constructs a DeploymentTarget for the current deployment attempt.
func (s *StageManager) deploymentFailureTarget() docker.DeploymentTarget {
	return docker.DeploymentTarget{
		Context:      s.DeployConfig.Context,
		Stack:        s.DeployConfig.Name,
		Repository:   s.Repository.Name,
		ConfigSource: retrySourceIdentity(s.Repository.ConfigSourceUrl),
		ConfigTarget: s.DeployConfig.Internal.ConfigTarget,
	}
}

// deploymentDesiredState computes the desired state of the deployment based on the current configuration and repository state.
func (s *StageManager) deploymentDesiredState() (docker.DeploymentDesiredState, error) {
	cfg := *s.DeployConfig
	// Reconciliation injects ForceRecreate for observed drift. It must not
	// turn the same failed release into a new desired state.
	cfg.ForceRecreate = false

	cfg.Context = docker.NormalizeContextName(cfg.Context)
	if s.Repository.Source != config.SourceTypeOCI {
		// Webhooks/polls can spell the same resolved branch differently. Match
		// the branch equivalence used by init without conflating explicit tags.
		cfg.Reference = strings.TrimPrefix(strings.TrimSpace(cfg.Reference), git.BranchPrefix)
	}

	data, err := json.Marshal(&cfg)
	if err != nil {
		return docker.DeploymentDesiredState{}, fmt.Errorf("hash retry deployment configuration: %w", err)
	}

	desired := docker.DeploymentDesiredState{
		Revision:    strings.TrimSpace(s.Repository.Revision),
		ConfigHash:  fmt.Sprintf("%x", sha256.Sum256(data)),
		ProjectHash: s.Docker.ProjectHash,
	}
	if s.retryDesired != nil {
		desired.ImageDigests = s.retryDesired.ImageDigests
	}

	return desired, nil
}

// logAdmittedDeploymentRetry logs information about a deployment retry that has been admitted.
func (s *StageManager) logAdmittedDeploymentRetry(ctx context.Context, stageLog *slog.Logger) {
	failure, failed := s.lastDeploymentFailure()
	if !failed {
		return
	}

	origin := s.Origin
	if origin == "" {
		origin = syncwindow.OriginAutomatic
	}

	stageLog.Log(ctx, slog.LevelWarn, "last deployment attempt failed, retrying",
		slog.String("failed_stage", failure.Stage),
		slog.String("failed_commit", failure.CommitSHA),
		slog.Time("failed_at", failure.FailedAt),
		slog.String("error", failure.Error),
		slog.Group("retry",
			slog.Int("used", failure.RetriesUsed),
			slog.Any("limit", s.DeployConfig.Retry.Limit),
			slog.String("origin", string(origin)),
		),
	)
}

// prepareDeploymentRetry runs on every mutating pre-deploy exit, including
// first deployments and OCI, before mutation admission and notifications.
func (s *StageManager) prepareDeploymentRetry(ctx context.Context, stageLog *slog.Logger) error {
	if s.Docker.Project == nil {
		if _, err := s.loadComposeProjectHash(ctx); err != nil {
			return err
		}
	}

	if s.Docker.ProjectHash == "" {
		hashProject := docker.WithNormalizedEnvValues(s.Docker.Project,
			pkiRoleNormMap(s.DeployConfig.ExternalSecrets, s.DeployConfig.Internal.Environment))

		hash, err := docker.ProjectHash(hashProject, s.Repository.PathExternal)
		if err != nil {
			return fmt.Errorf("hash retry compose project: %w", err)
		}

		s.Docker.ProjectHash = hash
	}

	desired, err := s.deploymentDesiredState()
	if err != nil {
		return err
	}

	if s.DeployConfig.Retry.Configured() && s.DeployConfig.ForceImagePull && desired.ImageDigests == nil {
		desired.ImageDigests = docker.DesiredProjectImageDigests(ctx, s.Docker.Cmd, s.Docker.Project, stageLog)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	s.retryDesired = &desired
	manual := s.Origin == syncwindow.OriginManual

	attempt, suppressed := docker.DeploymentFailures.Admit(s.deploymentFailureTarget(), desired, s.DeployConfig.Retry, manual)
	if suppressed == nil {
		s.retryAttempt = attempt
		if manual {
			if failure, failed := s.lastDeploymentFailure(); failed {
				stageLog.Info("manual deployment bypasses retry delay and budget",
					slog.Group("retry",
						slog.String("origin", string(s.Origin)),
						slog.Int("used", failure.RetriesUsed),
					),
				)
			}
		}

		return nil
	}

	level := slog.LevelDebug
	if suppressed.First {
		level = slog.LevelWarn
	}

	origin := s.Origin
	if origin == "" {
		origin = syncwindow.OriginAutomatic
	}

	retryAttrs := []any{
		slog.String("reason", suppressed.Reason),
		slog.Int("used", suppressed.Failure.RetriesUsed),
		slog.Any("limit", suppressed.RetryLimit),
	}
	message := "automatic deployment retries paused"

	if !suppressed.NextRetry.IsZero() {
		retryAttrs = append(retryAttrs, slog.Time("after", suppressed.NextRetry))
		message = "automatic deployment retry delayed"
	}

	attrs := []slog.Attr{
		slog.String("stack", s.DeployConfig.Name),
		slog.String("context", docker.DisplayContextName(s.DeployConfig.Context)),
		slog.String("revision", desired.Revision),
		slog.String("origin", string(origin)),
		slog.String("failed_stage", suppressed.Failure.Stage),
		slog.String("failure_reason", suppressed.Failure.Error),
		slog.Group("retry", retryAttrs...),
	}

	stageLog.LogAttrs(ctx, level, message, attrs...)

	return &RetrySuppressedError{Stacks: []RetrySuppressedStack{{
		Name:          s.DeployConfig.Name,
		Context:       docker.NormalizeContextName(s.DeployConfig.Context),
		Revision:      desired.Revision,
		Reason:        suppressed.Reason,
		NextRetry:     suppressed.NextRetry,
		FailedStage:   suppressed.Failure.Stage,
		FailureReason: suppressed.Failure.Error,
		RetriesUsed:   suppressed.Failure.RetriesUsed,
		RetryLimit:    suppressed.RetryLimit,
	}}}
}
