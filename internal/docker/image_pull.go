package docker

import (
	"context"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"

	"github.com/kimdre/doco-cd/internal/docker/imagepull"
)

const (
	imagePullAttempts = 4
	imagePullDelay    = 2 * time.Second
)

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
		retry.RetryIf(imagepull.IsTransientError),
	).Do(pull)
}
