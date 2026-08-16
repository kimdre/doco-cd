package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/lock"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/webhook"
)

// ErrCertRotationSwarmUnsupported is returned by RotateProjectCertificates when called for a
// deployment running in Docker Swarm mode, which is not yet supported.
var ErrCertRotationSwarmUnsupported = errors.New("automatic certificate rotation redeploy is not yet supported in Docker Swarm mode")

// certRotationTrigger is the value stamped into DocoCDLabels.Deployment.Trigger by rotation-driven
// redeploys, distinguishing them from webhook, poll, or scheduled-job triggered redeploys.
const certRotationTrigger = "cert.rotation"

// RotateProjectCertificates reloads the deploy config for a rotatable deployment (identified by its
// doco-cd labels, see DocoCDLabels.Deployment.CertRotatable), re-resolves its external secrets
// (which reissues any pki-role certificates through the configured secret provider), and forces a
// recreate of the services that consume the rotated certificates and their matching private keys,
// so the freshly-issued values take effect. Services in the same project that don't reference any
// pki-role-backed secret are left untouched.
//
// Only standalone Docker Compose deployments are currently supported; Swarm-mode stacks return
// ErrCertRotationSwarmUnsupported.
func RotateProjectCertificates(
	ctx context.Context,
	dockerCli command.Cli,
	labels map[string]string,
	secretProvider *secretprovider.SecretProvider,
	swarmMode bool,
) error {
	if swarmMode {
		return ErrCertRotationSwarmUnsupported
	}

	ref, err := composeScheduledServiceRefFromLabels(labels)
	if err != nil {
		return fmt.Errorf("parse deployment labels for cert rotation: %w", err)
	}

	stackName := strings.TrimSpace(labels[DocoCDLabels.Deployment.Name])
	if stackName == "" {
		stackName = ref.Project
	}

	lock.LockStack(stackName)
	defer lock.UnlockStack(stackName)

	project, deployConfig, err := loadComposeScheduledProjectAll(ctx, dockerCli, ref, secretProvider)
	if err != nil {
		return fmt.Errorf("reload deploy config for cert rotation of %s: %w", ref.Project, err)
	}

	// Only recreate the services that actually consume a pki-role-backed certificate or private
	// key, so unrelated services in the same project aren't disrupted by a rotation they have no
	// part in. If we can't determine any affected service (e.g. an unexpected project structure),
	// fall back to recreating every service rather than silently doing nothing after certificates
	// have already been reissued.
	serviceNames := servicesUsingRotatableCerts(project, deployConfig)
	if len(serviceNames) == 0 {
		serviceNames = make([]string, 0, len(project.Services))
		for name := range project.Services {
			serviceNames = append(serviceNames, name)
		}
	}

	selectedProject, err := project.WithSelectedServices(serviceNames, types.IgnoreDependencies)
	if err != nil {
		return fmt.Errorf("select certificate-consuming services for rotation of %s: %w", ref.Project, err)
	}

	// Build a synthetic payload for relabeling purposes only. CommitSHA is intentionally left as
	// the zero value: ParsedPayload.TriggerString() falls back to CommitSHAString(), which itself
	// returns "" for a zero hash instead of panicking, but we set Trigger explicitly anyway so the
	// resulting label clearly identifies this as a rotation-driven redeploy rather than an empty
	// commit SHA.
	payload := &webhook.ParsedPayload{
		Source:   webhook.PayloadSourceGit,
		Trigger:  certRotationTrigger,
		FullName: strings.TrimSpace(labels[DocoCDLabels.Source.Name]),
		WebURL:   strings.TrimSpace(labels[DocoCDLabels.Source.URL]),
	}

	timestamp := time.Now().UTC().Format(time.RFC3339)
	latestCommit := strings.TrimSpace(labels[DocoCDLabels.Deployment.CommitSHA])
	projectHash := strings.TrimSpace(labels[DocoCDLabels.Deployment.ComposeHash])

	addComposeServiceLabels(selectedProject, deployConfig, payload, ref.WorkingDir, app.Version, timestamp, ComposeVersion, latestCommit, projectHash)

	if err = deployCompose(ctx, dockerCli, selectedProject, deployConfig, api.RecreateForce, serviceNames, nil, func(string) {}); err != nil {
		return fmt.Errorf("redeploy project %s for cert rotation: %w", ref.Project, err)
	}

	return nil
}
