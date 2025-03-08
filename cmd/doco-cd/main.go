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
)

var (
	Version string
	errMsg  string
)

func main() {
	var wg sync.WaitGroup
	// Set default log level to debug
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

	h := handlerData{
		dockerCli: dockerCli,
		appConfig: c,
		log:       log,
	}

	http.HandleFunc(webhookPath, h.WebhookHandler)
	http.HandleFunc(webhookPath+"/{customTarget}", h.WebhookHandler)

	http.HandleFunc(healthPath, h.HealthCheckHandler)

	log.Info(
		"listening for events",
		slog.Int("http_port", int(c.HttpPort)),
		slog.String("path", webhookPath),
	)

	// RETRIEVE AND RE-LAUNCH CLEANUP PROCESSES IN CASE DOCO HAS RESTARTED
	dockerClient, _ := client.NewClientWithOpts(client.FromEnv)
	containers, err := docker.GetLabeledContainers(context.TODO(), dockerClient, "owner", "doco-cd")
	if err != nil {
		log.Error(fmt.Sprintf("failed to retrieve doco-cd containers: %s", err.Error()))
	}

	for _, cont := range containers {
		dir := cont.Labels["dir"]
		if len(dir) <= 0 {
			log.Error(fmt.Sprintf("failed to retrieve container %v tmp directory for cleanup", cont.ID))
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()

			docker.OnCrash(
				dockerCli.Client(),
				cont.ID,
				func() {
					log.Info("cleaning up", slog.String("path", dir))
					os.RemoveAll(dir)
				},
				func(err error) { log.Error("failed to clean up path: "+dir, logger.ErrAttr(err)) },
			)
		}()
	}

	err = http.ListenAndServe(fmt.Sprintf(":%d", c.HttpPort), nil)
	if err != nil {
		log.Error(fmt.Sprintf("failed to listen on port: %v", c.HttpPort), logger.ErrAttr(err))
	}

	wg.Wait()
}