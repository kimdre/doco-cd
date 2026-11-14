package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/lifecycle"
	"github.com/kimdre/doco-cd/internal/common/types/clone"
)

const (
	// SwarmHandoverLabel correlates a service update with its reporting record.
	// Service annotations do not change the task template.
	SwarmHandoverLabel  = "cd.doco.self.swarm.handover"
	swarmServiceIDLabel = "cd.doco.self.swarm.service_id"
	swarmStateLabel     = "cd.doco.self.swarm.state"
	swarmOwnerLabel     = "cd.doco.self.swarm.owner_task"
	swarmErrorLabel     = "cd.doco.self.swarm.error"
	swarmTimedOutLabel  = "cd.doco.self.swarm.timed_out"
)

// SwarmHandover survives task replacement and rollback independently of the service.
// Its Docker config has no stack namespace label, so stack config pruning excludes it.
type SwarmHandover struct {
	Record            Record          `json:"record"`
	ServiceID         string          `json:"service_id"`
	PredecessorTaskID string          `json:"predecessor_task_id"`
	ServiceVersion    uint64          `json:"service_version"`
	TaskTemplateHash  string          `json:"task_template_hash"`
	DesiredImage      string          `json:"desired_image"`
	Recovery          json.RawMessage `json:"recovery,omitempty"`
	Config            swarm.Config    `json:"-"`
	OwnerTaskID       string          `json:"-"`
}

func swarmHandoverName(serviceID string) (string, error) {
	if serviceID == "" || strings.ContainsAny(serviceID, "/\\ \t\r\n") {
		return "", errors.New("self-update: Swarm handover needs a valid service ID")
	}

	return "doco-cd-self-update-" + serviceID, nil
}

// CreateSwarmHandover reserves one durable reporting record per self service.
func CreateSwarmHandover(ctx context.Context, apiClient client.APIClient, handover *SwarmHandover) error {
	if handover.Record.ID == "" || handover.PredecessorTaskID == "" || handover.TaskTemplateHash == "" {
		return errors.New("self-update: Swarm handover is missing its update or task identity")
	}

	name, err := swarmHandoverName(handover.ServiceID)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	handover.Record.Version = RecordVersion
	handover.Record.State = StateHandover
	handover.Record.Strategy = StrategySwarm
	handover.Record.CreatedAt = now
	handover.Record.UpdatedAt = now

	handover.Record.Source = *clone.New(&handover.Record.Source)

	if err := sanitizeReportingURL(&handover.Record.Source.SourceURL); err != nil {
		return err
	}

	if info := handover.Record.Source.CommitStatus; info != nil {
		for _, value := range []*string{&info.SourceURL, &info.RepoURL} {
			if err := sanitizeReportingURL(value); err != nil {
				return err
			}
		}
	}

	data, err := json.Marshal(handover)
	if err != nil {
		return fmt.Errorf("encode Swarm handover: %w", err)
	}

	spec := swarm.ConfigSpec{
		Name: name,
		Labels: map[string]string{
			SwarmHandoverLabel:  handover.Record.ID,
			swarmServiceIDLabel: handover.ServiceID,
			swarmStateLabel:     string(StateHandover),
		},
		Data: data,
	}

	result, err := apiClient.ConfigCreate(ctx, client.ConfigCreateOptions{Spec: spec})
	if err != nil {
		return fmt.Errorf("create Swarm handover: %w", err)
	}

	handover.Config = swarm.Config{ID: result.ID, Spec: spec}

	return nil
}

// LoadSwarmHandover reads the self service's pending reporting record.
func LoadSwarmHandover(ctx context.Context, apiClient client.APIClient, serviceID string) (*SwarmHandover, error) {
	name, err := swarmHandoverName(serviceID)
	if err != nil {
		return nil, err
	}

	result, err := apiClient.ConfigInspect(ctx, name, client.ConfigInspectOptions{})
	if errdefs.IsNotFound(err) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("inspect Swarm handover: %w", err)
	}

	var handover SwarmHandover
	if err := json.Unmarshal(result.Config.Spec.Data, &handover); err != nil {
		return nil, fmt.Errorf("decode Swarm handover: %w", err)
	}

	if handover.Record.Version != RecordVersion || handover.Record.Strategy != StrategySwarm ||
		handover.ServiceID != serviceID || handover.Record.ID == "" || handover.PredecessorTaskID == "" ||
		handover.TaskTemplateHash == "" || handover.DesiredImage == "" ||
		result.Config.Spec.Labels[SwarmHandoverLabel] != handover.Record.ID ||
		result.Config.Spec.Labels[swarmServiceIDLabel] != serviceID {
		return nil, errors.New("self-update: Swarm handover identity does not match its Docker config")
	}

	handover.Config = result.Config
	handover.OwnerTaskID = result.Config.Spec.Labels[swarmOwnerLabel]
	handover.Record.State = State(result.Config.Spec.Labels[swarmStateLabel])
	handover.Record.Error = result.Config.Spec.Labels[swarmErrorLabel]
	handover.Record.TimedOut = result.Config.Spec.Labels[swarmTimedOutLabel] == "true"

	if handover.Record.State != StateHandover && handover.Record.State != StateFinalising {
		return nil, fmt.Errorf("invalid Swarm handover state %q", handover.Record.State)
	}

	return &handover, nil
}

// ClaimSwarmHandover uses Docker's version check to select one reporting task.
// The result stays durable when posting the final commit status fails.
func ClaimSwarmHandover(ctx context.Context, apiClient client.APIClient, handover SwarmHandover, taskID string, resultErr error) error {
	spec := clone.New(&handover.Config.Spec)
	spec.Labels[swarmStateLabel] = string(StateFinalising)
	spec.Labels[swarmOwnerLabel] = taskID
	delete(spec.Labels, swarmErrorLabel)
	delete(spec.Labels, swarmTimedOutLabel)

	if resultErr != nil {
		spec.Labels[swarmErrorLabel] = resultErr.Error()
		spec.Labels[swarmTimedOutLabel] = strconv.FormatBool(lifecycle.IsTimeout(resultErr))
	}

	_, err := apiClient.ConfigUpdate(ctx, handover.Config.ID, client.ConfigUpdateOptions{
		Version: handover.Config.Version,
		Spec:    *spec,
	})
	if err != nil {
		return fmt.Errorf("claim Swarm handover reporting: %w", err)
	}

	return nil
}

// RemoveSwarmHandover releases the reporting record only after its outcome is known.
func RemoveSwarmHandover(ctx context.Context, apiClient client.APIClient, handover SwarmHandover) error {
	_, err := apiClient.ConfigRemove(ctx, handover.Config.ID, client.ConfigRemoveOptions{})
	if err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove Swarm handover: %w", err)
	}

	return nil
}

// SwarmTaskAlive includes startup states, not only running tasks.
func SwarmTaskAlive(task swarm.Task) bool {
	switch task.Status.State {
	case swarm.TaskStateComplete, swarm.TaskStateShutdown, swarm.TaskStateFailed,
		swarm.TaskStateRejected, swarm.TaskStateRemove, swarm.TaskStateOrphaned:
		return false
	default:
		return true
	}
}

// SwarmUpdateError rejects rollbacks and revisions which replaced the pending update.
func SwarmUpdateError(handover SwarmHandover, service swarm.Service) error {
	if status := service.UpdateStatus; status != nil {
		switch status.State {
		case swarm.UpdateStateRollbackStarted, swarm.UpdateStateRollbackPaused, swarm.UpdateStateRollbackCompleted,
			swarm.UpdateStatePaused:
			return fmt.Errorf("self-update: Swarm entered %s: %s", status.State, status.Message)
		}
	}

	if service.Spec.Labels[SwarmHandoverLabel] != handover.Record.ID {
		if service.Version.Index <= handover.ServiceVersion {
			return errors.New("self-update: Swarm did not accept the self-service update")
		}

		return errors.New("self-update: Swarm update was superseded before confirmation")
	}

	hash, err := SwarmTaskSpecHash(service.Spec.TaskTemplate)
	if err != nil {
		return err
	}

	if hash != handover.TaskTemplateHash {
		return errors.New("self-update: Swarm task specification was superseded before confirmation")
	}

	desired, err := reference.ParseNormalizedNamed(handover.DesiredImage)
	if err != nil {
		return fmt.Errorf("parse desired Swarm self-update image: %w", err)
	}

	if pinned, ok := desired.(reference.Canonical); ok {
		current, err := reference.ParseNormalizedNamed(service.Spec.TaskTemplate.ContainerSpec.Image)
		if err != nil {
			return fmt.Errorf("parse current Swarm self-update image: %w", err)
		}

		currentPinned, ok := current.(reference.Canonical)
		if !ok || pinned.Digest() != currentPinned.Digest() {
			return errors.New("self-update: Swarm image was superseded before confirmation")
		}
	}

	return nil
}

// SwarmTaskSpecHash ignores image metadata Docker resolves from the registry.
// Only the hash is persisted, not environment values or other task configuration.
func SwarmTaskSpecHash(spec swarm.TaskSpec) (string, error) {
	if spec.ContainerSpec == nil {
		return "", errors.New("self-update: Swarm task has no container specification")
	}

	copySpec := clone.New(&spec)

	// Docker fills these defaults when it accepts the service specification.
	if copySpec.Runtime == "" {
		copySpec.Runtime = "container"
	}

	if copySpec.ContainerSpec.Isolation == "" {
		copySpec.ContainerSpec.Isolation = "default"
	}

	image, err := reference.ParseNormalizedNamed(copySpec.ContainerSpec.Image)
	if err != nil {
		return "", fmt.Errorf("parse Swarm self-update image: %w", err)
	}

	copySpec.ContainerSpec.Image = image.Name()
	if tagged, ok := image.(reference.Tagged); ok {
		copySpec.ContainerSpec.Image += ":" + tagged.Tag()
	} else if _, pinned := image.(reference.Canonical); !pinned {
		copySpec.ContainerSpec.Image += ":latest"
	}

	if copySpec.Placement != nil {
		copySpec.Placement.Platforms = nil
		if len(copySpec.Placement.Constraints) == 0 && len(copySpec.Placement.Preferences) == 0 &&
			copySpec.Placement.MaxReplicas == 0 {
			copySpec.Placement = nil
		}
	}

	data, err := json.Marshal(copySpec)
	if err != nil {
		return "", fmt.Errorf("hash Swarm self-update task: %w", err)
	}

	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// SwarmTaskMatches requires a running task with the current service's exact resolved image.
func SwarmTaskMatches(handover SwarmHandover, service swarm.Service, task swarm.Task) (bool, error) {
	if task.ServiceID != handover.ServiceID || task.ID == handover.PredecessorTaskID ||
		task.DesiredState != swarm.TaskStateRunning || task.Status.State != swarm.TaskStateRunning {
		return false, nil
	}

	hash, err := SwarmTaskSpecHash(task.Spec)
	if err != nil {
		return false, err
	}

	return hash == handover.TaskTemplateHash &&
		task.Spec.ContainerSpec.Image == service.Spec.TaskTemplate.ContainerSpec.Image, nil
}

func sanitizeReportingURL(value *string) error {
	if !strings.HasPrefix(*value, "http://") && !strings.HasPrefix(*value, "https://") {
		return nil
	}

	parsed, err := url.Parse(*value)
	if err != nil {
		return errors.New("invalid HTTP URL in Swarm handover reporting metadata")
	}

	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	*value = parsed.String()

	return nil
}
