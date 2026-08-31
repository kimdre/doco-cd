package reconciliation

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	composeapi "github.com/docker/compose/v5/pkg/api"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/moby/moby/api/types/container"
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

	err := manager.deploy(
		t.Context(),
		logger.New(logger.LevelCritical).Logger,
		nil,
		container.MountPoint{},
		nil,
		nil,
		nil,
		notification.Metadata{},
		stages.JobTriggerWebhook,
		stages.RepositoryData{
			Source:     config.SourceTypeOCI,
			SourceUrl:  "ghcr.io/example/repo:latest",
			OCITrusted: false,
		},
		nil,
		nil,
		"",
	)
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
	manager := newTestManager(t)

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

	tmpDir := t.TempDir()
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

	stackName := test.ConvertTestName(t.Name())

	dcs, err := deployConfig.GetConfigs(repoPath, c.DeployConfigBaseDir, "", p.Ref, nil)

	// commit have 5 apps
	// https://github.com/kimdre/doco-cd_tests/blob/7be81e788a40724cee7542eec00a2af0c4340eba/.doco-cd.yml
	for _, dc := range dcs {
		dc.Name = stackName + "-" + dc.Name
	}

	dcs[0].Reconciliation.Enabled = false
	dcs[1].Reconciliation.Events = []string{"stop"}

	// The default reconciliation events don't include "die".
	// Explicitly enable it for dcs[2..4] so the test's forceful container
	// removal (which emits a "die" event) triggers reconciliation as expected.
	for _, dc := range dcs[2:] {
		dc.Reconciliation.Events = []string{"die"}
	}

	t.Cleanup(func() {
		for _, dc := range dcs {
			waitForStackDeploymentToFinish(t, manager, repoName, dc.Context, dc.Name, 20*time.Second)
		}

		for _, dc := range dcs {
			ctx := context.Background()
			if err := destroyTestStack(ctx, dockerCli.Client(), dc.Name); err != nil {
				t.Error("destroyTestStack err", err)
			}
		}
	})

	if err := manager.Deploy(ctx, log, c,
		container.MountPoint{
			Type:        "bind",
			Source:      tmpDir,
			Destination: tmpDir,
			Mode:        "rw",
		},
		dockerCli,
		nil,
		secretProvider,
		notification.Metadata{
			JobID:      jobId,
			Repository: repoName,
			Revision:   notification.GetRevision(p.Ref, p.CommitSHAString()),
		},
		stages.JobTriggerWebhook,
		stages.RepositoryData{
			SourceUrl:    p.CloneURL,
			Name:         repoName,
			PathInternal: repoPath,
			PathExternal: repoPath,
		},
		dcs,

		&p,
		"",
	); err != nil {
		t.Fatalf("Failed to deploy: %v", err)
	}

	wanted := []string{}
	for _, dc := range dcs {
		wanted = append(wanted, dc.Name+"-test-1")
	}

	firstPartWanted := []string{wanted[2], wanted[3], wanted[4]}

	slices.Sort(wanted)

	waitForRunningContainerNames(ctx, t, dockerCli.Client(), stackName, wanted, 20*time.Second)
	waitForReconciliationJobReady(t, manager, repoName, 5*time.Second)

	if err := rmContainer(ctx, t, dockerCli.Client(), wanted); err != nil {
		t.Fatal("rm container err:", err)
	}

	waitForRunningContainerNames(ctx, t, dockerCli.Client(), stackName, nil, 20*time.Second)
	waitForRunningContainerNames(ctx, t, dockerCli.Client(), stackName, firstPartWanted, 20*time.Second)
}

func getRunningContainerNames(ctx context.Context, cli client.APIClient, stackName string) ([]string, error) {
	result, err := cli.ContainerList(ctx, client.ContainerListOptions{
		All: false,
	})
	if err != nil {
		return nil, err
	}

	stackContainerPrefix := stackName + "-"

	got := []string{}

	for _, c := range result.Items {
		name := strings.TrimPrefix(c.Names[0], "/")
		if strings.HasPrefix(name, stackContainerPrefix) {
			got = append(got, name)
		}
	}

	slices.Sort(got)

	return got, nil
}

func rmContainer(ctx context.Context, t *testing.T, cli client.APIClient, containerNames []string) error {
	wg := sync.WaitGroup{}
	for _, containerName := range containerNames {
		wg.Add(1)

		go func(name string) {
			defer wg.Done()

			_, err := cli.ContainerRemove(ctx, name, client.ContainerRemoveOptions{
				Force: true,
			})
			if err != nil {
				t.Errorf("rm container %s err: %v", name, err)
			}
		}(containerName)
	}

	wg.Wait()

	return nil
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

func waitForRunningContainerNames(ctx context.Context, t *testing.T, cli client.APIClient, stackName string, want []string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for {
		got, err := getRunningContainerNames(ctx, cli, stackName)
		if err != nil {
			t.Fatal("get containers err:", err)
		}

		if slices.Equal(want, got) {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for running containers %v, got %v", want, got)
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

	if err := docker.RemoveLabeledVolumes(ctx, cli, dockerSwarm.GetModeEnabled(), stackName); err != nil {
		return err
	}

	return nil
}
