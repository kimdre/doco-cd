package docker

import (
	"context"
	"strings"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
)

const (
	imagePullAttempts = 4
	imagePullDelay    = 2 * time.Second
)

// isTransientPullError reports whether an image pull failed because the daemon lost a
// content lease. This happens when "docker image prune" runs during a pull
// on the containerd image store (moby/moby#53321). A new pull succeeds.
func isTransientPullError(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "lease does not exist") || strings.Contains(msg, "failed commit on ref")
}

// pullProjectImages pulls the images of a project.
// It retries only when the daemon drops the content lease of the pull.
func pullProjectImages(ctx context.Context, service api.Compose, project *types.Project, opts api.PullOptions) error {
	return retryImagePull(ctx, imagePullDelay, func() error {
		return service.Pull(ctx, project, opts)
	})
}

func retryImagePull(ctx context.Context, delay time.Duration, pull func() error) error {
	return retry.New(
		retry.Context(ctx),
		retry.Attempts(imagePullAttempts),
		retry.Delay(delay),
		retry.DelayType(retry.BackOffDelay),
		retry.LastErrorOnly(true),
		retry.RetryIf(isTransientPullError),
	).Do(pull)
}
