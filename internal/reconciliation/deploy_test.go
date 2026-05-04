package reconciliation

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	composeapi "github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/encryption"
	"github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/notification"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/secretprovider/bitwardensecretsmanager"
	"github.com/kimdre/doco-cd/internal/stages"
	"github.com/kimdre/doco-cd/internal/test"
	"github.com/kimdre/doco-cd/internal/utils/id"
	"github.com/kimdre/doco-cd/internal/webhook"
)

func TestDeploy(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	ctx := t.Context()

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

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

	jobId := id.GenID()

	p := webhook.ParsedPayload{
		Ref:       "7be81e788a40724cee7542eec00a2af0c4340eba",
		CommitSHA: "7be81e788a40724cee7542eec00a2af0c4340eba",
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

	dcs, err := config.GetDeployConfigs(repoPath, c.DeployConfigBaseDir, stackName, "", p.Ref)

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
			waitForStackDeploymentToFinish(t, repoName, dc.Name, 20*time.Second)
		}

		reconciliationHandler.close()

		for _, dc := range dcs {
			ctx := context.Background()
			if err := destroyTestStack(ctx, dockerCli.Client(), dc.Name); err != nil {
				t.Error("destroyTestStack err", err)
			}
		}
	})

	if err := Deploy(ctx, log, c,
		container.MountPoint{
			Type:        "bind",
			Source:      tmpDir,
			Destination: tmpDir,
			Mode:        "rw",
		},
		dockerCli,
		&secretProvider,
		notification.Metadata{
			JobID:      jobId,
			Repository: repoName,
			Revision:   notification.GetRevision(p.Ref, p.CommitSHA),
		},
		stages.JobTriggerWebhook,
		stages.RepositoryData{
			CloneURL:     config.HttpUrl(p.CloneURL),
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

	got, err := getRunningContainerNames(ctx, dockerCli.Client(), stackName)
	if err != nil {
		t.Fatal("get containers err:", err)
	}

	if !reflect.DeepEqual(wanted, got) {
		t.Fatalf("first get running , expected %v, got %v", wanted, got)
	}

	// Give the reconciliation event listener a moment to subscribe before deleting containers.
	time.Sleep(time.Second)

	if err := rmContainer(ctx, t, dockerCli.Client(), wanted); err != nil {
		t.Fatal("rm container err:", err)
	}

	got, err = getRunningContainerNames(ctx, dockerCli.Client(), stackName)
	if err != nil {
		t.Fatal("get containers err:", err)
	}

	if !reflect.DeepEqual([]string{}, got) {
		t.Fatalf("rm container, get containers, expected empty, got %v", got)
	}

	deadline := time.Now().Add(20 * time.Second)

	for {
		got, err = getRunningContainerNames(ctx, dockerCli.Client(), stackName)
		if err != nil {
			t.Fatal("get containers err:", err)
		}

		if reflect.DeepEqual(firstPartWanted, got) {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("start +20s, get containers, expected %v, got %v", firstPartWanted, got)
		}

		time.Sleep(250 * time.Millisecond)
	}
}

func getRunningContainerNames(ctx context.Context, cli client.APIClient, prefix string) ([]string, error) {
	result, err := cli.ContainerList(ctx, client.ContainerListOptions{
		All: false,
	})
	if err != nil {
		return nil, err
	}

	got := []string{}

	for _, c := range result.Items {
		name := strings.TrimPrefix(c.Names[0], "/")
		if strings.HasPrefix(name, prefix) {
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

func waitForStackDeploymentToFinish(t *testing.T, repository, stack string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for {
		if !reconciliationHandler.isStackDeploymentInProgress(repository, stack) {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for reconciliation deployment to finish for stack %q in repository %q", stack, repository)
		}

		time.Sleep(100 * time.Millisecond)
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

	if err := docker.RemoveLabeledVolumes(ctx, cli, stackName); err != nil {
		return err
	}

	return nil
}
