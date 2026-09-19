package source

import (
	"context"
	"fmt"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/stages"
)

// resolveDeployConfigs resolves the deployment configuration(s) for req.
// Webhooks prefer centrally supplied deployments and otherwise read
// .doco-cd(.<target>).y(a)ml; polls resolve their optionally inline config.
//
// discoveryPath is the directory auto-discovery scans for compose files
// (the store's published artifact for req.Ref, for Git sources). gitMirrorDir
// and primaryRevision are Git-only and both empty for OCI: they let
// auto-discovery resolve a config's own Reference override (or the
// remote-repository branch) without requiring discoveryPath itself to be a
// git repository - see deploy.GetConfigs.
func (p *Preparer) resolveDeployConfigs(ctx context.Context, req Request, discoveryPath, gitMirrorDir, primaryRevision, ref string) ([]*deploy.Config, error) {
	gitOpts := &deploy.GitOptions{
		SSHPrivateKey:           p.appConfig.SSHPrivateKey,
		SSHPrivateKeyPassphrase: p.appConfig.SSHPrivateKeyPassphrase,
		GitAccessToken:          p.appConfig.GitAccessToken,
		SkipTLSVerification:     p.appConfig.SkipTLSVerification,
		HttpProxy:               p.appConfig.HttpProxy,
		GitCloneSubmodules:      p.appConfig.GitCloneSubmodules,
		GitCloneDepth:           p.appConfig.GitCloneDepth,
		SourceURL:               req.SourceRef,
		SourceBaseDir:           req.DataMountPoint.Destination,
	}

	switch req.JobTrigger {
	case stages.JobTriggerWebhook:
		if len(req.Deployments) > 0 {
			deployConfigs, err := deploy.ResolveConfigs(ctx, req.Deployments, req.CustomTarget, req.Ref, discoveryPath, p.appConfig.DeployConfigBaseDir, gitMirrorDir, primaryRevision, gitOpts)
			if err != nil {
				return nil, wrapPrepareError(ErrDeployConfig, err)
			}

			return deployConfigs, nil
		}

		deployConfigs, err := deploy.GetConfigs(ctx, discoveryPath, p.appConfig.DeployConfigBaseDir, req.CustomTarget, ref, gitMirrorDir, primaryRevision, gitOpts)
		if err != nil {
			return nil, wrapPrepareError(ErrDeployConfig, err)
		}

		return deployConfigs, nil
	case stages.JobTriggerPoll:
		deployConfigs, err := deploy.ResolveConfigs(ctx, req.PollConfig.Deployments, req.PollConfig.CustomTarget, req.Ref, discoveryPath, p.appConfig.DeployConfigBaseDir, gitMirrorDir, primaryRevision, gitOpts)
		if err != nil {
			return nil, wrapPrepareError(ErrDeployConfig, err)
		}

		return deployConfigs, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedJobTrigger, req.JobTrigger)
	}
}
