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

	"github.com/kimdre/doco-cd/internal/git/ssh"
	"github.com/kimdre/doco-cd/internal/graceful"

	"github.com/kimdre/doco-cd/internal/reconciliation"

	"github.com/kimdre/doco-cd/cmd/doco-cd/healthcheck"
	"github.com/kimdre/doco-cd/internal/secretprovider"

	"github.com/kimdre/doco-cd/internal/docker/swarm"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/prometheus"
)

const (
	apiPath     = "/v1/api"
	webhookPath = "/v1/webhook"
	healthPath  = "/v1/health"
	dataPath    = "/data"
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

func main() {
	// split to app to make defer work when os.Exit().
	if err := app(); err != nil {
		slog.Error("application stopped with error", logger.ErrAttr(err))
		os.Exit(1)
	}

	slog.Info("application stopped normally")
}

func app() error {
	ctx, rootCancel := context.WithCancel(context.Background())

	defer rootCancel()

	// Set the default log level to debug
	log := logger.New(slog.LevelDebug)

	// Get the application configuration
	c, err := config.GetAppConfig()
	if err != nil {
		log.Critical("failed to get application configuration", logger.ErrAttr(err))
		return err
	}

	// Parse the log level from the app configuration
	logLevel, err := logger.ParseLevel(c.LogLevel)
	if err != nil {
		logLevel = slog.LevelInfo
	}

	// Set the actual log level
	log = logger.New(logLevel)

	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		checkUrl := fmt.Sprintf("http://localhost:%d%s", c.HttpPort, healthPath)

		err := healthcheck.Check(ctx, checkUrl)
		if err != nil {
			log.Critical("health check failed", logger.ErrAttr(err), slog.String("url", checkUrl))
			return err
		}

		log.Info("health check successful", slog.String("url", checkUrl))

		return nil
	}

	log.Info("starting application", slog.String("version", config.AppVersion), slog.String("log_level", c.LogLevel))

	prometheus.AppInfo.WithLabelValues(config.AppVersion, c.LogLevel, time.Now().Format(time.RFC3339)).Set(1)

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

	if c.DockerSwarmFeatures {
		if err := swarm.RefreshModeEnabled(ctx, dockerClient); err != nil {
			log.Critical("failed to check if docker daemon is a swarm manager", logger.ErrAttr(err))
			return err
		}
	} else {
		swarm.SetDisableSwarmFeature(true)
		log.Debug("swarm features disabled by configuration")
	}

	log.Debug("negotiated docker versions to use",
		slog.Group("versions",
			slog.String("docker_client", dockerClient.ClientVersion()),
			slog.String("docker_api", dockerCli.CurrentVersion()),
			slog.Bool("swarm_mode", swarm.GetModeEnabled()),
		))

	// Get doco-cd container id
	appContainerID, err := getAppContainerID()
	if err != nil {
		log.Critical("failed to retrieve doco-cd container id", logger.ErrAttr(err))

		return err
	}

	log.Debug("retrieved doco-cd container id", slog.String("container_id", appContainerID))

	// Check if the doco-cd container has a data mount point and get the host path
	dataMountPoint, err := docker.GetMountPointByDestination(dockerClient, appContainerID, dataPath)
	if err != nil {
		log.Critical(fmt.Sprintf("failed to retrieve %s mount point for container %s", dataPath, appContainerID), logger.ErrAttr(err))
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
		log.Critical(fmt.Sprintf("failed to check if %s mount point is writable", dataPath), logger.ErrAttr(err))
		return err
	}

	if err := CreateMountpointSymlink(dataMountPoint); err != nil {
		log.Critical(fmt.Sprintf("failed to create symlink for %s mount point", dataMountPoint.Destination), logger.ErrAttr(err))

		return err
	}

	var wg sync.WaitGroup
	defer wg.Wait()

	graceful.SafeGo(&wg, log.Logger,
		func() {
			notificationForNewAppVersion(log.Logger)
		},
	)

	// Initialize SSH agent if SSH private key is provided
	if c.SSHPrivateKey != "" {
		ssh.RegisterSSHAgent(ctx, log.Logger, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase)
	}

	// Initialize the secret provider
	secretProvider, err := secretprovider.Initialize(ctx, c.SecretProvider, config.AppVersion)
	if err != nil {
		log.Critical("failed to initialize secret provider", logger.ErrAttr(err))

		return err
	}

	if secretProvider != nil {
		defer secretProvider.Close()

		log.Info("secret provider initialized", slog.String("provider", secretProvider.Name()))
	}

	h := handlerData{
		appConfig:      c,
		appVersion:     config.AppVersion,
		dataMountPoint: dataMountPoint,
		dockerCli:      dockerCli,
		log:            log,
		secretProvider: &secretProvider,
	}

	// Initialize the deployer limiter according to configuration
	reconciliation.InitializeDeployerLimiter(c.MaxConcurrentDeployments)

	if len(c.PollConfig) > 0 {
		// cancel poll jobs
		defer rootCancel()

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

	registryApiServer(c, &h, log)
	prometheus.RegisterServer(c.MetricsPort, log)

	if err := graceful.Serve(log.Logger); err != nil {
		log.Critical("failed to serve", logger.ErrAttr(err))
		return err
	}

	return nil
}
