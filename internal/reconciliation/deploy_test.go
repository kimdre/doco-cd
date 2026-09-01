package reconciliation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/cli/cli/command"
	composeapi "github.com/docker/compose/v5/pkg/api"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/moby/moby/api/types/container"
	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/common/id"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	dockerSwarm "github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/secretprovider/bitwardensecretsmanager"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/test"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestDeploy_RejectsUnverifiedOCIArtifact(t *testing.T) {
	t.Parallel()

	manager := newTestManager(t)

	err := manager.deploy(t.Context(), DeployRequest{
		Logger:     logger.New(logger.LevelCritical).Logger,
		JobTrigger: stages.JobTriggerWebhook,
		Repository: stages.RepositoryData{
			Source:     config.SourceTypeOCI,
			SourceUrl:  "ghcr.io/example/repo:latest",
			OCITrusted: false,
		},
	})
	if err == nil {
		t.Fatal("expected deploy to fail for unverified OCI artifact")
	}

	if !errors.Is(err, ErrOCIArtifactNotVerified) {
		t.Fatalf("expected ErrOCIArtifactNotVerified, got %v", err)
	}
}

func TestGroupDeployConfigsByMode(t *testing.T) {
	t.Parallel()

	compose := deployConfig.New("compose", "main")
	swarm := deployConfig.New("swarm", "main")
	automatic := deployConfig.New("automatic", "main")
	composeEnabled := false
	swarmEnabled := true
	compose.Swarm.Enabled = &composeEnabled
	swarm.Swarm.Enabled = &swarmEnabled

	grouped := groupDeployConfigsByMode([]*deployConfig.Config{compose, swarm, automatic}, true)
	if got := grouped[false]; len(got) != 1 || got[0] != compose {
		t.Fatalf("compose group = %#v, want only explicit compose config", got)
	}

	if got := grouped[true]; len(got) != 2 || got[0] != swarm || got[1] != automatic {
		t.Fatalf("swarm group = %#v, want explicit and automatic swarm configs", got)
	}

	unavailable := groupDeployConfigsByMode([]*deployConfig.Config{compose, swarm, automatic}, false)
	if got := unavailable[false]; len(got) != 2 || got[0] != compose || got[1] != automatic {
		t.Fatalf("unavailable swarm compose group = %#v, want explicit compose and automatic config", got)
	}

	if len(unavailable[true]) != 0 {
		t.Fatalf("unavailable swarm group = %#v, want no valid swarm config", unavailable[true])
	}
}

func TestDeploy(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	ctx := t.Context()

	c, err := app.GetConfig()
	if err != nil {
		t.Fatal(err)
	}

	c.GitCommitStatus = false

	log := logger.New(logger.LevelCritical).Logger

	dockerCli, err := docker.CreateDockerCli(c.DockerQuietDeploy)
	if err != nil {
		t.Fatalf("Failed to create docker client: %v", err)
	}

	t.Cleanup(func() {
		err = dockerCli.Client().Close()
		if err != nil {
			t.Log("Failed to close docker client:", err)
			return
		}
	})

	secretProvider, err := secretprovider.Initialize(ctx, c.SecretProvider, "v0.0.0-test")
	if err != nil {
		if errors.Is(err, bitwardensecretsmanager.ErrNotSupported) {
			t.Skip(err.Error())
		}

		t.Fatalf("failed to initialize secret provider: %s", err.Error())

		return
	}

	if secretProvider != nil {
		t.Cleanup(func() {
			secretProvider.Close()
		})
	}

	jobId := id.New()

	p := webhook.ParsedPayload{
		Ref:       "7be81e788a40724cee7542eec00a2af0c4340eba",
		CommitSHA: plumbing.NewHash("7be81e788a40724cee7542eec00a2af0c4340eba"),
		FullName:  "kimdre/doco-cd_tests",
		CloneURL:  "https://github.com/kimdre/doco-cd_tests.git",
		Private:   false,
	}
	swarmMode := resolveTestSwarmMode(t, dockerCli.Client())

	tmpDir := t.TempDir()

	manager := newTestManagerWithDependencies(t, Dependencies{
		AppConfig: c,
		DataMountPoint: container.MountPoint{
			Type:        "bind",
			Source:      tmpDir,
			Destination: tmpDir,
			Mode:        "rw",
		},
		DockerCLI:      dockerCli,
		SecretProvider: secretProvider,
	})

	// Use a test-unique repository name so this test's reconciliation job key does not
	// collide with other package tests that may run in parallel.
	repoName := test.ConvertTestName(t.Name()) + "-repo"
	repoPath := filepath.Join(tmpDir, repoName)

	_, err = git.CloneOrUpdateRepository(log, p.CloneURL, p.Ref,
		repoPath, repoPath,
		p.Private, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken, c.SkipTLSVerification,
		c.HttpProxy, c.GitCloneSubmodules, 0)
	if err != nil {
		t.Fatal(err)
	}

	if swarmMode {
		makeDeployFixtureSwarmCompatible(t, repoPath)
	}

	stackName := test.ConvertTestName(t.Name())

	dcs, err := deployConfig.GetConfigs(repoPath, c.DeployConfigBaseDir, "", p.Ref, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(dcs) != 5 {
		t.Fatalf("expected five deployment configs, got %d", len(dcs))
	}

	for _, dc := range dcs {
		dc.Name = stackName + "-" + dc.Name
	}

	dcs[0].Reconciliation.Enabled = false
	if swarmMode {
		// Service removal emits "remove", normalized to "destroy". App2 subscribes
		// to a different service event so only app3 through app5 are redeployed.
		dcs[1].Reconciliation.Events = []string{"update"}
		for _, dc := range dcs[2:] {
			dc.Reconciliation.Events = []string{"destroy"}
		}
	} else {
		// Force-removing a container emits "die", not "stop".
		dcs[1].Reconciliation.Events = []string{"stop"}
		for _, dc := range dcs[2:] {
			dc.Reconciliation.Events = []string{"die"}
		}
	}

	t.Cleanup(func() {
		for _, dc := range dcs {
			waitForStackDeploymentToFinish(t, manager, repoName, dc.Context, dc.Name, 20*time.Second)
		}

		manager.Close()

		for _, dc := range dcs {
			ctx := context.Background()
			if swarmMode {
				if err := removeTestSwarmStack(ctx, dockerCli, dc.Name); err != nil {
					t.Error("removeTestSwarmStack err", err)
				}
			} else if err := destroyTestStack(ctx, dockerCli.Client(), dc.Name); err != nil {
				t.Error("destroyTestStack err", err)
			}
		}
	})

	if err := manager.Deploy(ctx, DeployRequest{
		Logger: log,
		Metadata: notification.Metadata{
			JobID:      jobId,
			Repository: repoName,
			Revision:   notification.GetRevision(p.Ref, p.CommitSHAString()),
		},
		JobTrigger: stages.JobTriggerWebhook,
		Repository: stages.RepositoryData{
			SourceUrl:    p.CloneURL,
			Name:         repoName,
			PathInternal: repoPath,
			PathExternal: repoPath,
		},
		DeployConfigs: dcs,
		Payload:       &p,
	}); err != nil {
		t.Fatalf("Failed to deploy: %v", err)
	}

	wanted := make([]string, 0, len(dcs))
	for _, dc := range dcs {
		wanted = append(wanted, dc.Name)
	}

	firstPartWanted := []string{wanted[2], wanted[3], wanted[4]}

	slices.Sort(wanted)

	waitForRunningDeploymentNames(ctx, t, dockerCli.Client(), swarmMode, stackName, wanted, 20*time.Second)
	waitForReconciliationJobReady(t, manager, repoName, 5*time.Second)

	if swarmMode {
		removeSwarmServices(ctx, t, dockerCli.Client(), dcs)
	} else {
		if err := rmContainersForDeployments(ctx, t, dockerCli.Client(), wanted); err != nil {
			t.Fatal("rm container err:", err)
		}

		waitForRunningDeploymentNames(ctx, t, dockerCli.Client(), false, stackName, nil, 20*time.Second)
	}

	reconciliationTimeout := 20 * time.Second
	if swarmMode {
		reconciliationTimeout = 60 * time.Second
	}

	waitForRunningDeploymentNames(ctx, t, dockerCli.Client(), swarmMode, stackName, firstPartWanted, reconciliationTimeout)
}

func makeDeployFixtureSwarmCompatible(t *testing.T, repoPath string) {
	t.Helper()

	for _, relativePath := range []string{
		"deploy/app1/docker-compose.yml",
		"deploy/app2/docker-compose.yml",
	} {
		path := filepath.Join(repoPath, relativePath)

		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read Compose fixture %q: %v", relativePath, err)
		}

		updated := strings.ReplaceAll(string(contents), "    scale: 1\n", "")
		if updated == string(contents) {
			t.Fatalf("Compose fixture %q no longer contains the expected Swarm-incompatible scale option", relativePath)
		}

		// #nosec G703: relativePath comes from the fixed test fixture list above.
		if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
			t.Fatalf("failed to update Compose fixture %q: %v", relativePath, err)
		}
	}
}

func getRunningDeploymentNames(ctx context.Context, cli client.APIClient, swarmMode bool, stackName string) ([]string, error) {
	if swarmMode {
		result, err := cli.ServiceList(ctx, client.ServiceListOptions{})
		if err != nil {
			return nil, err
		}

		names := make(map[string]struct{})

		for _, service := range result.Items {
			name := docker.SwarmServiceLabels(service)[docker.DocoCDLabels.Deployment.Name]
			if !strings.HasPrefix(name, stackName+"-") {
				continue
			}

			running, err := swarmServiceHasRunningTask(ctx, cli, service.ID)
			if err != nil {
				return nil, err
			}

			if running {
				names[name] = struct{}{}
			}
		}

		got := make([]string, 0, len(names))
		for name := range names {
			got = append(got, name)
		}

		slices.Sort(got)

		return got, nil
	}

	result, err := cli.ContainerList(ctx, client.ContainerListOptions{
		All: false,
	})
	if err != nil {
		return nil, err
	}

	names := make(map[string]struct{})

	for _, c := range result.Items {
		name := c.Labels[docker.DocoCDLabels.Deployment.Name]
		if strings.HasPrefix(name, stackName+"-") {
			names[name] = struct{}{}
		}
	}

	got := make([]string, 0, len(names))
	for name := range names {
		got = append(got, name)
	}

	slices.Sort(got)

	return got, nil
}

func swarmServiceHasRunningTask(ctx context.Context, cli client.APIClient, serviceID string) (bool, error) {
	result, err := cli.TaskList(ctx, client.TaskListOptions{
		Filters: make(client.Filters).Add("service", serviceID),
	})
	if err != nil {
		return false, err
	}

	for _, task := range result.Items {
		if task.DesiredState == swarmTypes.TaskStateRunning && task.Status.State == swarmTypes.TaskStateRunning {
			return true, nil
		}
	}

	return false, nil
}

func rmContainersForDeployments(ctx context.Context, t *testing.T, cli client.APIClient, deploymentNames []string) error {
	result, err := cli.ContainerList(ctx, client.ContainerListOptions{
		All:     false,
		Filters: make(client.Filters).Add("label", docker.DocoCDLabels.Metadata.Manager+"="+app.Name),
	})
	if err != nil {
		return err
	}

	deployments := make(map[string]struct{}, len(deploymentNames))
	for _, name := range deploymentNames {
		deployments[name] = struct{}{}
	}

	wg := sync.WaitGroup{}

	for _, container := range result.Items {
		if _, ok := deployments[container.Labels[docker.DocoCDLabels.Deployment.Name]]; !ok {
			continue
		}

		wg.Add(1)

		go func(containerID string) {
			defer wg.Done()

			_, err := cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{
				Force: true,
			})
			if err != nil {
				t.Errorf("rm container %s err: %v", containerID, err)
			}
		}(container.ID)
	}

	wg.Wait()

	return nil
}

func removeSwarmServices(ctx context.Context, t *testing.T, cli client.APIClient, dcs []*deployConfig.Config) {
	t.Helper()

	for _, dc := range dcs {
		services, err := dockerSwarm.GetStackServices(ctx, cli, dc.Name)
		if err != nil {
			t.Fatalf("failed to list services for Swarm stack %q: %v", dc.Name, err)
		}

		for _, service := range services {
			if _, err := cli.ServiceRemove(ctx, service.ID, client.ServiceRemoveOptions{}); err != nil {
				t.Fatalf("failed to remove service %q from Swarm stack %q: %v", service.Spec.Name, dc.Name, err)
			}
		}
	}
}

func removeTestSwarmStack(ctx context.Context, dockerCli command.Cli, stackName string) error {
	var err error

	for range 5 {
		err = docker.RemoveSwarmStack(ctx, dockerCli, stackName)
		if err == nil {
			return nil
		}

		time.Sleep(250 * time.Millisecond)
	}

	return err
}

func waitForStackDeploymentToFinish(t *testing.T, manager *Manager, repository, context, stack string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for {
		if !manager.deployments.isInProgress(repository, context, stack) {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for reconciliation deployment to finish for stack %q in repository %q", stack, repository)
		}

		time.Sleep(100 * time.Millisecond)
	}
}

func waitForReconciliationJobReady(t *testing.T, manager *Manager, repository string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for {
		manager.jobs.mu.Lock()
		job := manager.jobs.jobs[repository]
		ready := job != nil && job.contextCLIs != nil
		manager.jobs.mu.Unlock()

		if ready {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for reconciliation job for repository %q to become ready", repository)
		}

		time.Sleep(100 * time.Millisecond)
	}
}

func waitForRunningDeploymentNames(ctx context.Context, t *testing.T, cli client.APIClient, swarmMode bool, stackName string, want []string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for {
		got, err := getRunningDeploymentNames(ctx, cli, swarmMode, stackName)
		if err != nil {
			t.Fatal("get deployments err:", err)
		}

		if slices.Equal(want, got) {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for running deployments %v, got %v", want, got)
		}

		time.Sleep(250 * time.Millisecond)
	}
}

func destroyTestStack(ctx context.Context, cli client.APIClient, stackName string) error {
	containers, err := docker.GetLabeledContainers(ctx, cli, composeapi.ProjectLabel, stackName, true)
	if err != nil {
		return err
	}

	for _, c := range containers {
		_, err = cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		if err != nil && !errdefs.IsNotFound(err) {
			return err
		}
	}

	networks, err := cli.NetworkList(ctx, client.NetworkListOptions{
		Filters: make(client.Filters).Add("label", composeapi.ProjectLabel+"="+stackName),
	})
	if err != nil {
		return err
	}

	for _, nw := range networks.Items {
		_, err = cli.NetworkRemove(ctx, nw.ID, client.NetworkRemoveOptions{})
		if err != nil && !errdefs.IsNotFound(err) {
			return err
		}
	}

	swarmMode, err := dockerSwarm.ResolveModeEnabled(ctx, cli)
	if err != nil {
		return err
	}

	if err := docker.RemoveLabeledVolumes(ctx, cli, swarmMode, stackName); err != nil {
		return err
	}

	return nil
}
