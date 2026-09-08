package source

import (
	"fmt"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/stages"
)

// resolveDeployConfigs resolves the deployment configuration(s) for req.
// Webhooks prefer centrally supplied deployments and otherwise read
// .doco-cd(.<target>).y(a)ml; polls resolve their optionally inline config.
func (p *Preparer) resolveDeployConfigs(req Request, internalRepoPath, ref string) ([]*deploy.Config, error) {
	gitOpts := &deploy.GitOptions{
		SSHPrivateKey:           p.appConfig.SSHPrivateKey,
		SSHPrivateKeyPassphrase: p.appConfig.SSHPrivateKeyPassphrase,
		GitAccessToken:          p.appConfig.GitAccessToken,
		SkipTLSVerification:     p.appConfig.SkipTLSVerification,
		HttpProxy:               p.appConfig.HttpProxy,
		GitCloneSubmodules:      p.appConfig.GitCloneSubmodules,
		GitCloneDepth:           p.appConfig.GitCloneDepth,
	}

	switch req.JobTrigger {
	case stages.JobTriggerWebhook:
		if len(req.Deployments) > 0 {
			deployConfigs, err := deploy.ResolveConfigs(req.Deployments, req.CustomTarget, req.Ref, internalRepoPath, p.appConfig.DeployConfigBaseDir, gitOpts)
			if err != nil {
				return nil, wrapPrepareError(ErrDeployConfig, err)
			}

			return deployConfigs, nil
		}

		deployConfigs, err := deploy.GetConfigs(internalRepoPath, p.appConfig.DeployConfigBaseDir, req.CustomTarget, ref, gitOpts)
		if err != nil {
			return nil, wrapPrepareError(ErrDeployConfig, err)
		}

		return deployConfigs, nil
	case stages.JobTriggerPoll:
		deployConfigs, err := deploy.ResolveConfigs(req.PollConfig.Deployments, req.PollConfig.CustomTarget, req.Ref, internalRepoPath, p.appConfig.DeployConfigBaseDir, gitOpts)
		if err != nil {
			return nil, wrapPrepareError(ErrDeployConfig, err)
		}

		return deployConfigs, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedJobTrigger, req.JobTrigger)
	}
}
