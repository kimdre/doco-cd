package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"

	"github.com/docker/docker/api/types/container"

	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/docker/docker/client"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
)

const (
	webhookPath = "/v1/webhook"
	healthPath  = "/v1/health"
	dataPath    = "/data"
)

var (
	Version string
	errMsg  string
)

// getAppContainerID retrieves the application container ID from the cpuset file
func getAppContainerID() (string, error) {
	const (
		cgroupMounts = "/proc/self/mountinfo"
		dockerPath   = "/docker/containers/"
		podmanPath   = "/containers/storage/overlay-containers/"
	)

	dockerPattern := regexp.MustCompile(dockerPath + `([a-z0-9]+)`)
	podmanPattern := regexp.MustCompile(podmanPath + `([a-z0-9]+)`)

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

		path := fields[3]

		if strings.Contains(line, "/etc/hostname") {
			if strings.Contains(path, dockerPath) {
				if matches := dockerPattern.FindStringSubmatch(path); len(matches) > 1 {
					return matches[1], nil
				}
			}

			if strings.Contains(path, podmanPath) {
				if matches := podmanPattern.FindStringSubmatch(path); len(matches) > 1 {
					return matches[1], nil
				}
			}
		}
	}

	return "", errors.New("container ID not found")
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
// Required so that the docker cli client is able to read/parse certain files (like .env files) in docker.LoadCompose
func CreateMountpointSymlink(m container.MountPoint) error {
	// prepare the symlink parent directory
	symlinkParentDir := path.Dir(m.Source)

	err := os.MkdirAll(symlinkParentDir, 0o755)
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

	log.Debug("docker client created")

	dockerClient, _ := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)

	log.Debug("negotiated docker versions to use",
		slog.Group("versions",
			slog.String("docker_client", dockerClient.ClientVersion()),
			slog.String("docker_api", dockerCli.CurrentVersion()),
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

	http.HandleFunc(webhookPath, h.WebhookHandler)
	http.HandleFunc(webhookPath+"/{customTarget}", h.WebhookHandler)

	http.HandleFunc(healthPath, h.HealthCheckHandler)

	log.Info(
		"listening for events",
		slog.Int("http_port", int(c.HttpPort)),
		slog.String("path", webhookPath),
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

	log.Debug("retrieving containers that are managed by doco-cd")

	containers, err := docker.GetLabeledContainers(context.TODO(), dockerClient, docker.DocoCDLabels.Metadata.Manager, config.AppName)
	if err != nil {
		log.Error("failed to retrieve doco-cd containers", logger.ErrAttr(err))
	}

	if len(containers) <= 0 {
		log.Debug("no containers found that are managed by doco-cd", slog.Int("count", len(containers)))
	} else {
		log.Debug("retrieved containers successfully", slog.Int("count", len(containers)))
	}

	for _, cont := range containers {
		log.Debug("inspecting container", slog.Group("container",
			slog.String("id", cont.ID),
			slog.String("name", cont.Names[0]),
		))

		dir := cont.Labels[docker.DocoCDLabels.Deployment.WorkingDir]
		if len(dir) <= 0 {
			log.Error(fmt.Sprintf("failed to retrieve container %v working directory", cont.ID))
			continue
		}

		wg.Add(1)

		go func() {
			defer wg.Done()
			// docker.OnCrash(
			//
			//	dockerCli.Client(),
			//	cont.ID,
			//	func() {
			//		log.Info("cleaning up", slog.String("path", dir))
			//		_ = os.RemoveAll(dir)
			//	},
			//	func(err error) { log.Error("failed to clean up path: "+dir, logger.ErrAttr(err)) },
			//
			// )
		}()
	}

	err = http.ListenAndServe(fmt.Sprintf(":%d", c.HttpPort), nil)
	if err != nil {
		log.Error(fmt.Sprintf("failed to listen on port: %v", c.HttpPort), logger.ErrAttr(err))
	}

	wg.Wait()
}
