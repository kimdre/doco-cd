package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/git"

	"github.com/kimdre/doco-cd/internal/git/ssh"
	"github.com/kimdre/doco-cd/internal/graceful"

	"github.com/kimdre/doco-cd/internal/reconciliation"

	"github.com/kimdre/doco-cd/cmd/doco-cd/healthcheck"
	"github.com/kimdre/doco-cd/internal/certrotation"
	"github.com/kimdre/doco-cd/internal/scheduler"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/secretprovider/openbao"

	"github.com/kimdre/doco-cd/internal/docker/swarm"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/registryauth"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/prometheus"
)

// GetProxyUrlRedacted takes a proxy URL string and redacts the password if it exists.
func GetProxyUrlRedacted(proxyUrl string) string {
	// Hide password in the proxy URL if it exists (between the second ':' and the @)
	if strings.Contains(proxyUrl, "@") {
		re := regexp.MustCompile(`://([^:]+):([^@]+)@`)
		proxyUrl = re.ReplaceAllString(proxyUrl, "://$1:***@")
	} else {
		re := regexp.MustCompile(`://([^@]+)@`)
		proxyUrl = re.ReplaceAllString(proxyUrl, "://$1@")
	}

	return proxyUrl
}

// CreateMountpointSymlink creates the Symlink for the data mount point to reflect the data volume path in the container.
// Required so that the docker cli client is able to read/parse certain files (like .env files) in docker.LoadCompose.
func CreateMountpointSymlink(m container.MountPoint) error {
	// if source ends with `/` path.Dir will like remove `/`,
	// like `/data/dococd/` -> /data/dococd which is not what we want.
	source := filepath.Clean(m.Source)
	destination := filepath.Clean(m.Destination)

	if source == destination {
		return nil
	}

	symlinkParentDir := filepath.Dir(source)

	err := os.MkdirAll(symlinkParentDir, filesystem.PermDir)
	if err != nil {
		return fmt.Errorf("failed to create parent directory %s: %w", symlinkParentDir, err)
	}

	err = os.Symlink(destination, source)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			// If the symlink already exists, we can ignore the error
			err = nil
		}

		return err
	}

	return nil
}

func resolveDataMountPoint(
	dataHostPath string,
	dataMountPath string,
	detectMountPoint func() (container.MountPoint, error),
) (container.MountPoint, error) {
	if dataHostPath != "" {
		return container.MountPoint{
			Type:        "bind",
			Source:      dataHostPath,
			Destination: dataMountPath,
			Mode:        "rw",
			RW:          true,
		}, nil
	}

	return detectMountPoint()
}

func detectDataMountPoint(
	dataMountPath string,
	getContainerID func() (string, error),
	getMountPoint func(string, string) (container.MountPoint, error),
) (container.MountPoint, error) {
	appContainerID, err := getContainerID()
	if err != nil {
		return container.MountPoint{}, fmt.Errorf("failed to retrieve doco-cd container id: %w", err)
	}

	mountPoint, err := getMountPoint(appContainerID, dataMountPath)
	if err != nil {
		return container.MountPoint{}, fmt.Errorf("failed to retrieve %s mount point for container %s: %w", dataMountPath, appContainerID, err)
	}

	return mountPoint, nil
}

func main() {
	// split to app to make defer work when os.Exit().
	if err := run(); err != nil {
		slog.Error("application stopped with error", logger.ErrAttr(err))
		os.Exit(1)
	}

	slog.Info("application stopped normally")
}

// run is the main entry point for the application.
// It initializes the application, sets up necessary resources, and starts the server.
func run() error {
	ctx, rootCancel := context.WithCancel(context.Background())

	defer rootCancel()

	// Set the default log level to debug
	log := logger.New(slog.LevelDebug)

	// Get the application configuration
	c, err := app.GetConfig()
	if err != nil {
		log.Critical("failed to get application configuration", logger.ErrAttr(err))
		return err
	}

	git.ConfigureAuthResolver(
		c.GitAuthDomains,
		c.SSHPrivateKey,
		c.SSHPrivateKeyPassphrase,
		c.GitAccessToken,
		c.GitAccessTokenUser,
		git.GitHubAppConfig{
			ID:             c.GitHubAppID,
			PrivateKey:     c.GitHubAppPrivateKey,
			InstallationID: c.GitHubAppInstallationID,
		},
	)

	// Parse the log level from the app configuration
	logLevel, err := logger.ParseLevel(c.LogLevel)
	if err != nil {
		logLevel = slog.LevelInfo
	}

	// Set the actual log level
	log = logger.New(logLevel)

	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		scheme := "http"
		if c.HttpTLSEnabled {
			scheme = "https"
		}

		checkUrl := fmt.Sprintf("%s://localhost:%d%s", scheme, c.HttpPort, healthPath)

		err := healthcheck.Check(ctx, checkUrl, c.HttpTLSEnabled)
		if err != nil {
			log.Critical("health check failed", logger.ErrAttr(err), slog.String("url", checkUrl))
			return err
		}

		log.Info("health check successful", slog.String("url", checkUrl))

		return nil
	}

	log.Info("starting application", slog.String("version", app.Version), slog.String("log_level", c.LogLevel))

	prometheus.AppInfo.WithLabelValues(app.Version, c.LogLevel, time.Now().Format(time.RFC3339)).Set(1)

	// Log if proxy is used
	if c.HttpProxy != (transport.ProxyOptions{}) {
		log.Info("using HTTP proxy", slog.String("url", GetProxyUrlRedacted(c.HttpProxy.URL)))
	} else {
		log.Debug("no HTTP proxy configured")
	}

	// Test/verify the connection to the docker socket
	err, errType := docker.VerifyDockerAPIAccess()
	if err != nil {
		log.Critical(errType.Error(), logger.ErrAttr(err))
		return err
	}

	log.Debug("connection to docker socket was successful")

	dockerCli, err := docker.CreateDockerCli(c.DockerQuietDeploy)
	if err != nil {
		log.Critical("failed to create docker client", logger.ErrAttr(err))

		return err
	}

	dockerClient := dockerCli.Client()

	defer func(client client.APIClient) {
		log.Debug("closing docker client")

		err = client.Close()
		if err != nil {
			log.Error("failed to close docker client", logger.ErrAttr(err))
		}
	}(dockerClient)

	if err = registryauth.ValidateDockerConfig(dockerCli.ConfigFile()); err != nil {
		log.Critical("docker config validation failed", logger.ErrAttr(err), slog.String("config_file", dockerCli.ConfigFile().Filename))
		return err
	}

	if missing := registryauth.MissingConfiguredCredentialHelpers(dockerCli.ConfigFile()); len(missing) > 0 {
		for _, m := range missing {
			log.Warn("missing credential helper binary in container; image pulls from affected registries will fail",
				slog.String("helper", m.Helper),
				slog.String("binary", m.Binary),
				slog.Any("affected_registries", m.Registries),
				slog.String("config_file", dockerCli.ConfigFile().Filename),
			)
		}
	}

	if c.DockerSwarmFeatures {
		if err := swarm.RefreshModeEnabled(ctx, dockerClient); err != nil {
			log.Critical("failed to check if docker daemon is a swarm manager", logger.ErrAttr(err))
			return err
		}
	} else {
		swarm.SetDisableSwarmFeature(true)
		log.Debug("swarm features disabled by configuration")
	}

	contexts := docker.NewContextRegistry(dockerCli, c.DockerQuietDeploy)
	defer func() {
		if closeErr := contexts.Close(); closeErr != nil {
			log.Error("failed to close docker context clients", logger.ErrAttr(closeErr))
		}
	}()

	log.Debug("negotiated docker versions to use",
		slog.Group("versions",
			slog.String("docker_client", dockerClient.ClientVersion()),
			slog.String("docker_api", dockerCli.CurrentVersion()),
			slog.Bool("swarm_mode", swarm.GetModeEnabled()),
		))

	dataMountPoint, err := resolveDataMountPoint(
		c.DataHostPath,
		c.DataMountPath,
		func() (container.MountPoint, error) {
			return detectDataMountPoint(
				c.DataMountPath,
				func() (string, error) {
					appContainerID, err := getAppContainerID()
					if err == nil {
						log.Debug("retrieved doco-cd container id", slog.String("container_id", appContainerID))
					}

					return appContainerID, err
				},
				func(containerID, destination string) (container.MountPoint, error) {
					return docker.GetMountPointByDestination(dockerClient, containerID, destination)
				},
			)
		},
	)
	if err != nil {
		log.Critical("failed to resolve doco-cd data mount point", logger.ErrAttr(err))
		return err
	}

	log.Debug("retrieved doco-cd data mount point",
		slog.Group("mount_point",
			slog.String("source", dataMountPoint.Source),
			slog.String("destination", dataMountPoint.Destination),
		),
	)

	// Check if data mount point is writable
	if err := docker.CheckMountPointWriteable(dataMountPoint); err != nil {
		log.Critical(fmt.Sprintf("failed to check if %s mount point is writable", c.DataMountPath), logger.ErrAttr(err))
		return err
	}

	if err := CreateMountpointSymlink(dataMountPoint); err != nil {
		log.Critical(fmt.Sprintf("failed to create symlink for %s mount point", dataMountPoint.Destination), logger.ErrAttr(err))

		return err
	}

	var wg sync.WaitGroup
	defer wg.Wait()
	// cancel the root context to signal all goroutines to stop,
	// avoid wg.wait hang infinitely.
	defer rootCancel()

	graceful.SafeGo(&wg, log.Logger,
		func() {
			notificationForNewAppVersion(log.Logger)
		},
	)

	// Initialize SSH agent with the global and domain scoped SSH keys, if any are configured
	sshKeys := []ssh.KeyRecord{{PrivateKey: c.SSHPrivateKey, Passphrase: c.SSHPrivateKeyPassphrase}}
	for _, scoped := range c.GitAuthDomains {
		sshKeys = append(sshKeys, ssh.KeyRecord{PrivateKey: scoped.SSHPrivateKey, Passphrase: scoped.SSHPrivateKeyPassphrase})
	}

	ssh.RegisterSSHAgent(ctx, log.Logger, sshKeys)

	// Initialize the secret provider
	secretProvider, err := secretprovider.Initialize(ctx, c.SecretProvider, app.Version)
	if err != nil {
		log.Critical("failed to initialize secret provider", logger.ErrAttr(err))

		return err
	}

	if secretProvider != nil {
		defer secretProvider.Close()

		log.Info("secret provider initialized", slog.String("provider", secretProvider.Name()))
	}

	schedulerManager := scheduler.NewManager(contexts, log.Logger, &wg, &secretProvider)

	h := handlerData{
		appConfig:      c,
		appVersion:     app.Version,
		dataMountPoint: dataMountPoint,
		dockerCli:      dockerCli,
		contexts:       contexts,
		log:            log,
		runTracker: newDeploymentRunTracker(map[deploymentRunTrigger]int{
			deploymentRunTriggerPoll:         50,
			deploymentRunTriggerWebhook:      50,
			deploymentRunTriggerScheduledJob: 50,
		}),
		scheduler:      schedulerManager,
		secretProvider: &secretProvider,
	}

	// Initialize the deployer limiter according to configuration
	reconciliation.InitializeDeployerLimiter(c.MaxConcurrentDeployments)

	if len(c.PollConfig) > 0 {
		log.Info(
			"poll configuration found, scheduling polling jobs",
			slog.Any("poll_config", logger.BuildSliceLogValue(c.PollConfig, "Deployments.Internal")),
		)

		for _, pollConfig := range c.PollConfig {
			err = StartPoll(ctx, &h, pollConfig, &wg)
			if err != nil {
				log.Critical("failed to scheduling polling jobs", logger.ErrAttr(err))

				return err
			}
		}
	}

	if c.SchedulerEnabled {
		graceful.SafeGo(&wg, log.Logger, func() {
			schedulerManager.Start(ctx)
		})
	} else {
		log.Info("scheduler disabled by configuration")
	}

	if c.CertRotationEnabled {
		if c.SecretProvider != openbao.Name {
			log.Warn(
				"certificate rotation is enabled but the configured secret provider does not support it, disabling",
				slog.String("secret_provider", c.SecretProvider),
			)
		} else {
			watcher := certrotation.New(contexts, log.Logger, h.secretProvider, c.CertRotationThreshold, c.CertRotationCheckInterval)

			graceful.SafeGo(&wg, log.Logger, func() {
				watcher.Start(ctx)
			})
		}
	} else if c.SecretProvider == openbao.Name {
		log.Info("certificate rotation watcher disabled by configuration")
	}

	registryApiServer(c, &h, log)
	prometheus.RegisterServer(c.MetricsPort, c.HttpTLSCertFile, c.HttpTLSKeyFile, log)

	if err := graceful.Serve(log.Logger); err != nil {
		log.Critical("failed to serve", logger.ErrAttr(err))
		return err
	}

	return nil
}
