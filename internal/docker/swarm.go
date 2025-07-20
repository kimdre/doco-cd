package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/client"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/command/stack/loader"
	"github.com/docker/cli/cli/command/stack/options"
	"github.com/docker/cli/cli/command/stack/swarm"
	composetypes "github.com/docker/cli/cli/compose/types"
	"github.com/spf13/pflag"

	"github.com/kimdre/doco-cd/internal/webhook"

	"github.com/kimdre/doco-cd/internal/config"
)

const (
	StackNamespaceLabel = "com.docker.stack.namespace"
)

var SwarmModeEnabled bool // Whether the docker host is running in swarm mode

// DeploySwarmStack deploys a Docker Swarm stack using the provided project and deploy configuration.
func DeploySwarmStack(ctx context.Context, dockerCli command.Cli, project *types.Project, deployConfig *config.DeployConfig,
	payload webhook.ParsedPayload, repoDir, latestCommit, appVersion string,
) error {
	opts := options.Deploy{
		Composefiles:     project.ComposeFiles,
		Namespace:        deployConfig.Name,
		ResolveImage:     swarm.ResolveImageAlways,
		SendRegistryAuth: false,
		Prune:            deployConfig.RemoveOrphans,
		Detach:           false,
		Quiet:            true,
	}

	timestamp := time.Now().UTC().Format(time.RFC3339)

	cfg, err := loader.LoadComposefile(dockerCli, opts)
	if err != nil {
		return fmt.Errorf("failed to load compose file: %w", err)
	}

	addSwarmServiceLabels(cfg, *deployConfig, payload, repoDir, appVersion, timestamp, latestCommit)
	addSwarmVolumeLabels(cfg, *deployConfig, payload, repoDir, appVersion, timestamp, latestCommit)
	addSwarmConfigLabels(cfg, *deployConfig, payload, repoDir, appVersion, timestamp, latestCommit)
	addSwarmSecretLabels(cfg, *deployConfig, payload, repoDir, appVersion, timestamp, latestCommit)

	if err = SetConfigHashPrefixes(cfg, opts.Namespace); err != nil {
		return fmt.Errorf("failed to set config hash prefixes: %w", err)
	}

	if err = SetSecretHashPrefixes(cfg, opts.Namespace); err != nil {
		return fmt.Errorf("failed to set secret hash prefixes: %w", err)
	}

	return swarm.RunDeploy(ctx, dockerCli, &pflag.FlagSet{}, &opts, cfg)
}

// RemoveSwarmStack removes a Docker Swarm stack using the provided deploy configuration.
func RemoveSwarmStack(ctx context.Context, dockerCli command.Cli, deployConfig *config.DeployConfig) error {
	opts := options.Remove{
		Namespaces: []string{deployConfig.Name},
		Detach:     false,
	}

	return swarm.RunRemove(ctx, dockerCli, opts)
}

// addSwarmServiceLabels adds custom labels to the service containers in a Docker Swarm stack.
func addSwarmServiceLabels(stack *composetypes.Config, deployConfig config.DeployConfig, payload webhook.ParsedPayload, repoDir, appVersion, timestamp, latestCommit string) {
	customLabels := map[string]string{
		DocoCDLabels.Metadata.Manager:      config.AppName,
		DocoCDLabels.Metadata.Version:      appVersion,
		DocoCDLabels.Deployment.Name:       deployConfig.Name,
		DocoCDLabels.Deployment.Timestamp:  timestamp,
		DocoCDLabels.Deployment.WorkingDir: repoDir,
		DocoCDLabels.Deployment.Trigger:    payload.CommitSHA,
		DocoCDLabels.Deployment.CommitSHA:  latestCommit,
		DocoCDLabels.Deployment.TargetRef:  deployConfig.Reference,
		DocoCDLabels.Repository.Name:       payload.FullName,
		DocoCDLabels.Repository.URL:        payload.WebURL,
	}

	for i, s := range stack.Services {
		if s.Labels == nil {
			s.Labels = make(map[string]string)
		}

		for key, val := range customLabels {
			s.Labels[key] = val
		}

		stack.Services[i] = s
	}
}

// addSwarmVolumeLabels adds custom labels to the volumes in a Docker Swarm stack.
func addSwarmVolumeLabels(stack *composetypes.Config, deployConfig config.DeployConfig, payload webhook.ParsedPayload, repoDir, appVersion, timestamp, latestCommit string) {
	customLabels := map[string]string{
		DocoCDLabels.Metadata.Manager:      config.AppName,
		DocoCDLabels.Metadata.Version:      appVersion,
		DocoCDLabels.Deployment.Name:       deployConfig.Name,
		DocoCDLabels.Deployment.Timestamp:  timestamp,
		DocoCDLabels.Deployment.WorkingDir: repoDir,
		DocoCDLabels.Deployment.Trigger:    payload.CommitSHA,
		DocoCDLabels.Deployment.CommitSHA:  latestCommit,
		DocoCDLabels.Deployment.TargetRef:  deployConfig.Reference,
		DocoCDLabels.Repository.Name:       payload.FullName,
		DocoCDLabels.Repository.URL:        payload.WebURL,
	}

	for i, v := range stack.Volumes {
		if v.Labels == nil {
			v.Labels = make(map[string]string)
		}

		for key, val := range customLabels {
			v.Labels[key] = val
		}

		stack.Volumes[i] = v
	}
}

// addSwarmConfigLabels adds custom labels to the configs in a Docker Swarm stack.
func addSwarmConfigLabels(stack *composetypes.Config, deployConfig config.DeployConfig, payload webhook.ParsedPayload, repoDir, appVersion, timestamp, latestCommit string) {
	customLabels := map[string]string{
		DocoCDLabels.Metadata.Manager:      config.AppName,
		DocoCDLabels.Metadata.Version:      appVersion,
		DocoCDLabels.Deployment.Name:       deployConfig.Name,
		DocoCDLabels.Deployment.Timestamp:  timestamp,
		DocoCDLabels.Deployment.WorkingDir: repoDir,
		DocoCDLabels.Deployment.Trigger:    payload.CommitSHA,
		DocoCDLabels.Deployment.CommitSHA:  latestCommit,
		DocoCDLabels.Deployment.TargetRef:  deployConfig.Reference,
		DocoCDLabels.Repository.Name:       payload.FullName,
		DocoCDLabels.Repository.URL:        payload.WebURL,
	}

	for i, c := range stack.Configs {
		if c.Labels == nil {
			c.Labels = make(map[string]string)
		}

		for key, val := range customLabels {
			c.Labels[key] = val
		}

		stack.Configs[i] = c
	}
}

func addSwarmSecretLabels(stack *composetypes.Config, deployConfig config.DeployConfig, payload webhook.ParsedPayload, repoDir, appVersion, timestamp, latestCommit string) {
	customLabels := map[string]string{
		DocoCDLabels.Metadata.Manager:      config.AppName,
		DocoCDLabels.Metadata.Version:      appVersion,
		DocoCDLabels.Deployment.Name:       deployConfig.Name,
		DocoCDLabels.Deployment.Timestamp:  timestamp,
		DocoCDLabels.Deployment.WorkingDir: repoDir,
		DocoCDLabels.Deployment.Trigger:    payload.CommitSHA,
		DocoCDLabels.Deployment.CommitSHA:  latestCommit,
		DocoCDLabels.Deployment.TargetRef:  deployConfig.Reference,
		DocoCDLabels.Repository.Name:       payload.FullName,
		DocoCDLabels.Repository.URL:        payload.WebURL,
	}

	for i, s := range stack.Secrets {
		if s.Labels == nil {
			s.Labels = make(map[string]string)
		}

		for key, val := range customLabels {
			s.Labels[key] = val
		}

		stack.Secrets[i] = s
	}
}

// SetConfigHashPrefixes generates hashes for the config definitions in the stack config
// and adds them to the config names as suffixes to trigger a redeployment when they change (Only works in Docker Swarm mode).
func SetConfigHashPrefixes(stack *composetypes.Config, namespace string) error {
	for i, c := range stack.Configs {
		if c.External.External {
			// Skip external configs, they are not managed by the stack
			continue
		}

		var content io.Reader

		contentBytes, err := os.ReadFile(c.File)
		if err != nil {
			return fmt.Errorf("failed to read config file %s: %w", c.File, err)
		}

		content = strings.NewReader(string(contentBytes))

		hash, err := generateShortHash(content)
		if err != nil {
			return fmt.Errorf("failed to generate hash for config %s: %w", c.Name, err)
		}

		if c.Name == "" {
			c.Name = fmt.Sprintf("%s_%s", namespace, filepath.Base(c.File))
		}

		oldName := c.Name
		nameWithHash := fmt.Sprintf("%s_%s", c.Name, hash)
		c.Name = nameWithHash
		stack.Configs[i] = c

		// Check for services that use this config and update their config references
		for j, service := range stack.Services {
			for k, cfg := range service.Configs {
				if cfg.Source == oldName {
					// Update the config reference in the service
					stack.Services[j].Configs[k].Source = nameWithHash
				}
			}
		}
	}

	return nil
}

// SetSecretHashPrefixes generates hashes for the secret definitions in the stack config
// and adds them to the secret names as suffixes to trigger a redeployment when they change (Only works in Docker Swarm mode).
func SetSecretHashPrefixes(stack *composetypes.Config, namespace string) error {
	for i, s := range stack.Secrets {
		if s.External.External {
			// Skip external secrets, they are not managed by the stack
			continue
		}

		var content io.Reader

		contentBytes, err := os.ReadFile(s.File)
		if err != nil {
			return fmt.Errorf("failed to read secret file %s: %w", s.File, err)
		}

		content = strings.NewReader(string(contentBytes))

		hash, err := generateShortHash(content)
		if err != nil {
			return fmt.Errorf("failed to generate hash for secret %s: %w", s.Name, err)
		}

		if s.Name == "" {
			s.Name = fmt.Sprintf("%s_%s", namespace, filepath.Base(s.File))
		}

		oldName := s.Name
		nameWithHash := fmt.Sprintf("%s_%s", s.Name, hash)
		s.Name = nameWithHash
		stack.Secrets[i] = s

		// Check for services that use this secret and update their secret references
		for j, service := range stack.Services {
			for k, secret := range service.Secrets {
				if secret.Source == oldName {
					// Update the secret reference in the service
					stack.Services[j].Secrets[k].Source = nameWithHash
				}
			}
		}
	}

	return nil
}

// generateShortHash generates a short hash from the provided data reader.
func generateShortHash(data io.Reader) (hash string, err error) {
	const length = 8

	h := sha256.New()

	_, err = io.Copy(h, data)
	if err != nil {
		return "", fmt.Errorf("failed to generate hash: %w", err)
	}

	hash = hex.EncodeToString(h.Sum(nil))
	if len(hash) > length {
		hash = hash[:length] // Shorten hash to n characters
	} else if hash == "" {
		return "", errors.New("empty hash")
	}

	return hash, nil
}

func CheckDaemonIsSwarmManager(ctx context.Context, dockerCli command.Cli) (bool, error) {
	info, err := dockerCli.Client().Info(ctx)
	if err != nil {
		return false, err
	}

	if !info.Swarm.ControlAvailable {
		return false, nil
	}

	return true, nil
}

func PruneStackConfigs(ctx context.Context, client *client.Client, namespace string) error {
	// List all configs in the swarm
	configs, err := GetLabeledConfigs(ctx, client, StackNamespaceLabel, namespace)
	if err != nil {
		return fmt.Errorf("failed to list configs: %w", err)
	}

	for _, c := range configs {
		if c.Spec.Labels[StackNamespaceLabel] == namespace {
			// Remove the c if it belongs to the specified namespace
			err = client.ConfigRemove(ctx, c.ID)
			if err != nil {
				if strings.Contains(err.Error(), ErrIsInUse.Error()) {
					// If the config is in use, we can skip it
					continue
				}

				return fmt.Errorf("failed to remove c %s: %w", c.ID, err)
			}
		}
	}

	return nil
}

func PruneStackSecrets(ctx context.Context, client *client.Client, namespace string) error {
	// List all secrets in the swarm
	secrets, err := GetLabeledSecrets(ctx, client, StackNamespaceLabel, namespace)
	if err != nil {
		return fmt.Errorf("failed to list secrets: %w", err)
	}

	for _, s := range secrets {
		if s.Spec.Labels[StackNamespaceLabel] == namespace {
			// Remove the secret if it belongs to the specified namespace
			err = client.SecretRemove(ctx, s.ID)
			if err != nil {
				if strings.Contains(err.Error(), ErrIsInUse.Error()) {
					// If the config is in use, we can skip it
					continue
				}

				return fmt.Errorf("failed to remove secret %s: %w", s.ID, err)
			}
		}
	}

	return nil
}
