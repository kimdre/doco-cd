package docker

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/moby/moby/client"
)

const (
	// TODO: Remove these labels and this migration file in a future release.
	legacyAutoDiscoverLabel        = "cd.doco.deployment.auto_discover"
	legacyAutoDiscoverDeleteLabel  = "cd.doco.deployment.auto_discover.delete"
	legacyAutoDiscoveryDeleteLabel = "cd.doco.deployment.auto_discovery.delete"
)

// GetAutoDiscoveryServices returns auto-discovered workloads and migrates legacy
// auto-discovery labels to the current representation. Swarm service labels can be
// updated in place; standalone containers persist the normalized labels during the
// deployment that follows reconciliation.
func GetAutoDiscoveryServices(ctx context.Context, cli client.APIClient, swarmMode bool) (map[Service]map[string]string, error) {
	if swarmMode {
		return getAndMigrateSwarmAutoDiscoveryServices(ctx, cli)
	}

	return getStandaloneAutoDiscoveryServices(ctx, cli)
}

func getAndMigrateSwarmAutoDiscoveryServices(ctx context.Context, cli client.APIClient) (map[Service]map[string]string, error) {
	response, err := cli.ServiceList(ctx, client.ServiceListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list services for auto-discovery label migration: %w", err)
	}

	result := make(map[Service]map[string]string)

	for _, service := range response.Items {
		labels := maps.Clone(service.Spec.Labels)
		if labels == nil {
			labels = make(map[string]string)
		}

		if containerSpec := service.Spec.TaskTemplate.ContainerSpec; containerSpec != nil {
			for key, value := range containerSpec.Labels {
				if _, exists := labels[key]; !exists {
					labels[key] = value
				}
			}
		}

		normalized, _ := normalizeAutoDiscoveryLabels(labels)
		if normalized == nil {
			continue
		}

		if needsSwarmAutoDiscoveryLabelMigration(service.Spec.Labels) {
			spec := service.Spec

			spec.Labels = maps.Clone(service.Spec.Labels)
			if spec.Labels == nil {
				spec.Labels = make(map[string]string)
			}

			spec.Labels[DocoCDLabels.Deployment.AutoDiscovery] = normalized[DocoCDLabels.Deployment.AutoDiscovery]
			spec.Labels[DocoCDLabels.Deployment.AutoDiscoveryConfig] = normalized[DocoCDLabels.Deployment.AutoDiscoveryConfig]
			delete(spec.Labels, legacyAutoDiscoverLabel)
			delete(spec.Labels, legacyAutoDiscoverDeleteLabel)
			delete(spec.Labels, legacyAutoDiscoveryDeleteLabel)

			if spec.Mode.ReplicatedJob != nil || spec.Mode.GlobalJob != nil {
				spec.UpdateConfig = nil
				spec.RollbackConfig = nil
			}

			if _, err := cli.ServiceUpdate(ctx, service.ID, client.ServiceUpdateOptions{
				Version: service.Version,
				Spec:    spec,
			}); err != nil {
				return nil, fmt.Errorf("failed to migrate auto-discovery labels for service %s: %w", service.Spec.Name, err)
			}
		}

		result[Service(service.Spec.Name)] = normalized
	}

	return result, nil
}

func needsSwarmAutoDiscoveryLabelMigration(labels map[string]string) bool {
	if _, exists := labels[DocoCDLabels.Deployment.AutoDiscovery]; !exists {
		return true
	}

	if _, exists := labels[DocoCDLabels.Deployment.AutoDiscoveryConfig]; !exists {
		return true
	}

	for _, label := range []string{
		legacyAutoDiscoverLabel,
		legacyAutoDiscoverDeleteLabel,
		legacyAutoDiscoveryDeleteLabel,
	} {
		if _, exists := labels[label]; exists {
			return true
		}
	}

	return false
}

func getStandaloneAutoDiscoveryServices(ctx context.Context, cli client.APIClient) (map[Service]map[string]string, error) {
	result := make(map[Service]map[string]string)

	for _, label := range []string{DocoCDLabels.Deployment.AutoDiscovery, legacyAutoDiscoverLabel} {
		services, err := GetLabeledServices(ctx, cli, false, label, "true")
		if err != nil {
			return nil, err
		}

		for service, labels := range services {
			normalized, _ := normalizeAutoDiscoveryLabels(labels)
			if normalized != nil {
				result[service] = normalized
			}
		}
	}

	return result, nil
}

func normalizeAutoDiscoveryLabels(labels map[string]string) (map[string]string, bool) {
	enabledValue, hasCurrentEnabled := labels[DocoCDLabels.Deployment.AutoDiscovery]
	if !hasCurrentEnabled {
		enabledValue = labels[legacyAutoDiscoverLabel]
	}

	enabled, err := strconv.ParseBool(strings.TrimSpace(enabledValue))
	if err != nil || !enabled {
		return nil, false
	}

	normalized := maps.Clone(labels)
	migrate := !hasCurrentEnabled

	normalized[DocoCDLabels.Deployment.AutoDiscovery] = strconv.FormatBool(enabled)

	if _, hasConfig := labels[DocoCDLabels.Deployment.AutoDiscoveryConfig]; !hasConfig {
		cfg := ParseAutoDiscoveryConfig("")
		cfg.Enabled = enabled

		legacyDelete := labels[legacyAutoDiscoveryDeleteLabel]
		if legacyDelete == "" {
			legacyDelete = labels[legacyAutoDiscoverDeleteLabel]
		}

		if deleteEnabled, err := strconv.ParseBool(strings.TrimSpace(legacyDelete)); err == nil {
			cfg.Delete = deleteEnabled
		}

		normalized[DocoCDLabels.Deployment.AutoDiscoveryConfig] = MarshalAutoDiscoveryConfig(cfg)
		migrate = true
	}

	for _, label := range []string{
		legacyAutoDiscoverLabel,
		legacyAutoDiscoverDeleteLabel,
		legacyAutoDiscoveryDeleteLabel,
	} {
		if _, exists := normalized[label]; exists {
			delete(normalized, label)

			migrate = true
		}
	}

	return normalized, migrate
}
