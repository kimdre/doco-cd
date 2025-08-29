package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/go-git/go-git/v5/plumbing/transport"

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

var (
	Version string
	errMsg  string
)

// getAppContainerID retrieves the application container ID from the cpuset file.
func getAppContainerID() (string, error) {
	const (
		cgroupMounts  = "/proc/self/mountinfo"
		containerPath = "/containers/"
	)

	containerPattern := regexp.MustCompile(containerPath + `([a-z0-9]+)`)

	data, err := os.ReadFile(cgroupMounts)
	if err != nil {
		return "", fmt.Errorf("failed to read %s: %w", cgroupMounts, err)
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		mountPath := fields[3]

		if strings.Contains(line, "/etc/hostname") {
			if strings.Contains(mountPath, containerPath) {
				if matches := containerPattern.FindStringSubmatch(mountPath); len(matches) > 1 {
					return matches[1], nil
				}
			}
		}
	}

	return "", docker.ErrContainerIDNotFound
}

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
	// prepare the symlink parent directory
	symlinkParentDir := path.Dir(m.Source)

	err := os.MkdirAll(symlinkParentDir, filesystem.PermDir)
	if err != nil {
		return fmt.Errorf("failed to create parent directory %s: %w", symlinkParentDir, err)
	}

	err = os.Symlink(m.Destination, m.Source)
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
	var wg sync.WaitGroup
	// Set the default log level to debug
	log := logger.New(slog.LevelDebug)

	// Get the application configuration
	c, err := config.GetAppConfig()
	if err != nil {
		log.Critical("failed to get application configuration", logger.ErrAttr(err))
	}

	// Parse the log level from the app configuration
	logLevel, err := logger.ParseLevel(c.LogLevel)
	if err != nil {
		logLevel = slog.LevelInfo
	}

	// Set the actual log level
	log = logger.New(logLevel)

	log.Info("starting application", slog.String("version", Version), slog.String("log_level", c.LogLevel))

	prometheus.AppInfo.WithLabelValues(Version, c.LogLevel, time.Now().Format(time.RFC3339)).Set(1)

	// Log if proxy is used
	if c.HttpProxy != (transport.ProxyOptions{}) {
		log.Info("using HTTP proxy", slog.String("url", GetProxyUrlRedacted(c.HttpProxy.URL)))
	} else {
		log.Debug("no HTTP proxy configured")
	}

	go func() {
		latestVersion, err := getLatestAppReleaseVersion()
		if err != nil {
			log.Error("failed to get latest application release version", logger.ErrAttr(err))
		} else {
			if Version != latestVersion {
				log.Warn("new application version available",
					slog.String("current", Version),
					slog.String("latest", latestVersion),
				)
			} else {
				log.Debug("application is up to date", slog.String("version", Version))
			}
		}
	}()

	// Test/verify the connection to the docker socket
	err = docker.VerifySocketConnection()
	if err != nil {
		log.Critical(docker.ErrDockerSocketConnectionFailed.Error(), logger.ErrAttr(err))
	}

	log.Debug("connection to docker socket was successful")

	dockerCli, err := docker.CreateDockerCli(c.DockerQuietDeploy, !c.SkipTLSVerification)
	if err != nil {
		log.Critical("failed to create docker client", logger.ErrAttr(err))

		return
	}
	defer func(client client.APIClient) {
		log.Debug("closing docker client")

		err = client.Close()
		if err != nil {
			log.Error("failed to close docker client", logger.ErrAttr(err))
		}
	}(dockerCli.Client())

	dockerClient, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		log.Critical("failed to create docker client", logger.ErrAttr(err))

		return
	}

	log.Debug("negotiated docker versions to use",
		slog.Group("versions",
			slog.String("docker_client", dockerClient.ClientVersion()),
			slog.String("docker_api", dockerCli.CurrentVersion()),
			slog.Bool("swarm_mode", docker.SwarmModeEnabled),
		))

	// Get container id of this application
	appContainerID, err := getAppContainerID()
	if err != nil {
		log.Critical("failed to retrieve application container id", logger.ErrAttr(err))

		return
	}

	log.Debug("retrieved application container id", slog.String("container_id", appContainerID))

	// Check if the application has a data mount point and get the host path
	dataMountPoint, err := docker.GetMountPointByDestination(dockerClient, appContainerID, dataPath)
	if err != nil {
		log.Critical(fmt.Sprintf("failed to retrieve %s mount point for container %s", dataPath, appContainerID), logger.ErrAttr(err))
	}

	log.Debug("retrieved data mount point",
		slog.Group("mount_point",
			slog.String("source", dataMountPoint.Source),
			slog.String("destination", dataMountPoint.Destination),
		),
	)

	// Check if data mount point is writable
	err = docker.CheckMountPointWriteable(dataMountPoint)
	if err != nil {
		log.Critical(fmt.Sprintf("failed to check if %s mount point is writable", dataPath), logger.ErrAttr(err))
	}

	err = CreateMountpointSymlink(dataMountPoint)
	if err != nil {
		log.Critical(fmt.Sprintf("failed to create symlink for %s mount point", dataMountPoint.Destination), logger.ErrAttr(err))

		return
	}

	h := handlerData{
		appConfig:      c,
		appVersion:     Version,
		dataMountPoint: dataMountPoint,
		dockerCli:      dockerCli,
		dockerClient:   dockerClient,
		log:            log,
	}

	// Register HTTP endpoints
	enabledEndpoints := registerHttpEndpoints(c, &h, log)

	log.Info(
		"listening for events",
		slog.Int("http_port", int(c.HttpPort)),
		slog.Any("enabled_endpoints", enabledEndpoints),
	)

	if len(c.PollConfig) > 0 {
		log.Info("poll configuration found, scheduling polling jobs", slog.Any("poll_config", c.PollConfig))

		for _, pollConfig := range c.PollConfig {
			err = StartPoll(&h, pollConfig, &wg)
			if err != nil {
				log.Critical("failed to scheduling polling jobs", logger.ErrAttr(err))

				return
			}
		}
	}

	go func() {
		log.Info("serving prometheus metrics", slog.Int("http_port", int(c.MetricsPort)), slog.String("path", prometheus.MetricsPath))

		if err = prometheus.Serve(c.MetricsPort); err != nil {
			log.Critical("failed to start Prometheus metrics server", logger.ErrAttr(err))
		} else {
			log.Debug("Prometheus metrics server started successfully", slog.Int("port", int(c.MetricsPort)))
		}
	}()

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", c.HttpPort),
		ReadHeaderTimeout: 3 * time.Second,
	}

	err = server.ListenAndServe()
	if err != nil {
		log.Error(fmt.Sprintf("failed to listen on port: %v", c.HttpPort), logger.ErrAttr(err))
	}

	wg.Wait()
}
