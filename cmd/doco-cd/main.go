package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"

	"github.com/docker/docker/client"
	"github.com/kimdre/doco-cd/internal/docker"

	"github.com/kimdre/doco-cd/internal/config"
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

// getAppContainerID retrieves the container ID of the application
func getAppContainerID() string {
	return os.Getenv("HOSTNAME")
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

	// Check if the application has a data mount point and get the host path
	appContainerID := getAppContainerID()
	log.Debug("retrieved application container id", slog.String("container_id", appContainerID))

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

	log.Debug("data mount point is writable")

	h := handlerData{
		appConfig:      c,
		dataMountPoint: dataMountPoint,
		dockerCli:      dockerCli,
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

	log.Debug("retrieving containers that are managed by doco-cd")

	containers, err := docker.GetLabeledContainers(context.TODO(), dockerClient, docker.DocoCDLabels.Metadata.Manager, "doco-cd")
	if err != nil {
		log.Error("failed to retrieve doco-cd containers", logger.ErrAttr(err))
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

	if len(containers) <= 0 {
		log.Debug("no containers found that are managed by doco-cd", slog.Int("count", len(containers)))
	} else {
		log.Debug("retrieved containers successfully", slog.Int("count", len(containers)))
	}

	err = http.ListenAndServe(fmt.Sprintf(":%d", c.HttpPort), nil)
	if err != nil {
		log.Error(fmt.Sprintf("failed to listen on port: %v", c.HttpPort), logger.ErrAttr(err))
	}

	wg.Wait()
}
