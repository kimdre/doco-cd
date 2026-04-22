package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/docker/cli/cli/command"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/reconciliation"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/webhook"
)

type handleError struct {
	msg string // errMsg
	err error  // detail err, can be nil if not applicable

	httpStatusCode int // http status code use to respond to http request
}

func (r handleError) Error() string {
	ret := r.msg

	if r.err != nil {
		ret += fmt.Sprintf(", err: %v", r.err)
	}

	return ret
}

func handle(ctx context.Context, jobLog *slog.Logger,
	appConfig *config.AppConfig,
	dataMountPoint container.MountPoint,
	secretProvider *secretprovider.SecretProvider,
	dockerCli command.Cli,
	jobTrigger stages.JobTrigger,
	cloneURL string, ref string, private bool,
	metadata notification.Metadata,
	customTarget string, testName string,
	pollConfig config.PollConfig,
	payload webhook.ParsedPayload,
) error {
	repoName := git.GetRepoName(cloneURL)

	jobLog = jobLog.With(
		slog.String("job_id", metadata.JobID),
		slog.String("repository", repoName),
	)

	if customTarget != "" {
		jobLog = jobLog.With(slog.String("custom_target", customTarget))
	}

	if strings.Contains(repoName, "..") {
		return handleError{
			err:            fmt.Errorf("invalid repository name: %s, contains '..'", repoName),
			msg:            "invalid repository name",
			httpStatusCode: http.StatusBadRequest,
		}
	}

	if err := swarm.RefreshModeEnabled(ctx, dockerCli.Client()); err != nil {
		return handleError{
			err:            err,
			msg:            "failed to check if docker host is running in swarm mode",
			httpStatusCode: http.StatusInternalServerError,
		}
	}

	// Path inside the container
	internalRepoPath, err := filesystem.VerifyAndSanitizePath(
		filepath.Join(dataMountPoint.Destination, repoName),
		dataMountPoint.Destination,
	)
	if err != nil {
		return handleError{
			err:            err,
			msg:            "failed to verify and sanitize internal filesystem path",
			httpStatusCode: http.StatusBadRequest,
		}
	}

	// Path on the host
	externalRepoPath, err := filesystem.VerifyAndSanitizePath(
		filepath.Join(dataMountPoint.Source, repoName),
		dataMountPoint.Source,
	)
	if err != nil {
		return handleError{
			err:            err,
			msg:            "failed to verify and sanitize external filesystem path",
			httpStatusCode: http.StatusBadRequest,
		}
	}

	if _, err := git.CloneOrUpdateRepository(jobLog,
		cloneURL, ref, internalRepoPath, externalRepoPath,
		private, appConfig.SSHPrivateKey, appConfig.SSHPrivateKeyPassphrase, appConfig.GitAccessToken,
		appConfig.SkipTLSVerification, appConfig.HttpProxy, appConfig.GitCloneSubmodules, appConfig.GitCloneDepth,
	); err != nil {
		return handleError{
			err:            err,
			msg:            "failed to clone repository",
			httpStatusCode: http.StatusInternalServerError,
		}
	}

	jobLog.Debug("retrieving deployment configuration")

	var deployConfigs []*config.DeployConfig

	switch jobTrigger {
	case stages.JobTriggerWebhook:
		// Get the deployment configs from the repository
		deployConfigs, err = config.GetDeployConfigs(internalRepoPath, appConfig.DeployConfigBaseDir, payload.Name, customTarget, payload.Ref)
		if err != nil {
			return handleError{
				err:            err,
				msg:            "failed to get deploy configuration",
				httpStatusCode: http.StatusInternalServerError,
			}
		}
	case stages.JobTriggerPoll:
		// shortName is the last part of repoName, which is just the name of the repository
		shortName := filepath.Base(repoName)

		// Resolve deployment configs (prefer inline in poll config when present)
		deployConfigs, err = config.ResolveDeployConfigs(pollConfig, internalRepoPath, appConfig.DeployConfigBaseDir, shortName)
		if err != nil {
			return handleError{
				err:            err,
				msg:            "failed to get deploy configuration",
				httpStatusCode: http.StatusInternalServerError,
			}
		}

	default:
		return handleError{
			err:            fmt.Errorf("unsupported job trigger: %s", jobTrigger),
			msg:            "unsupported job trigger",
			httpStatusCode: http.StatusBadRequest,
		}
	}

	repoData := stages.RepositoryData{
		CloneURL:     config.HttpUrl(cloneURL),
		Name:         repoName,
		PathInternal: internalRepoPath,
		PathExternal: externalRepoPath,
	}

	if err := reconciliation.Deploy(ctx, jobLog, appConfig,
		dataMountPoint, dockerCli, secretProvider, metadata, jobTrigger,
		repoData, deployConfigs, &payload, testName); err != nil {
		return handleError{
			err:            err,
			msg:            "deployment failed",
			httpStatusCode: http.StatusInternalServerError,
		}
	}

	return nil
}
