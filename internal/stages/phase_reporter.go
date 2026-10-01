package stages

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/common/lifecycle"
)

// phaseReportDwell is how long a deployment phase must last before it is
// published, so fast phases neither flicker in the Git UI nor spend requests
// against the provider's rate limit.
const phaseReportDwell = 3 * time.Second

// phasePoster publishes a deployment phase.
type phasePoster func(ctx context.Context, phase string) error

// phaseReporter publishes the phases of a running deployment in its commit
// status. It implements docker.PhaseReporter.
//
// Updates are best-effort: only the latest phase is published, once it has
// lasted for the dwell time, with a single attempt and at most one request in
// flight. A rate limit response disables further updates for the deployment.
type phaseReporter struct {
	log   *slog.Logger
	post  phasePoster
	dwell time.Duration

	mu     sync.Mutex
	latest string

	notify   chan struct{}
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// newPhaseReporter starts a reporter that publishes phases with post until
// Stop is called.
func newPhaseReporter(ctx context.Context, log *slog.Logger, dwell time.Duration, post phasePoster) *phaseReporter {
	r := &phaseReporter{
		log:    log,
		post:   post,
		dwell:  dwell,
		notify: make(chan struct{}, 1),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}

	go r.run(ctx)

	return r
}

// Report records the current phase. It never blocks.
func (r *phaseReporter) Report(phase string) {
	r.mu.Lock()
	r.latest = phase
	r.mu.Unlock()

	select {
	case r.notify <- struct{}{}:
	default:
	}
}

// Stop drops pending phases and waits for a request in flight, so nothing is
// published after it returns. It is safe to call more than once.
func (r *phaseReporter) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}

func (r *phaseReporter) current() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.latest
}

func (r *phaseReporter) stopped() bool {
	select {
	case <-r.stop:
		return true
	default:
		return false
	}
}

func (r *phaseReporter) run(ctx context.Context) {
	defer close(r.done)

	timer := time.NewTimer(r.dwell)
	timer.Stop()

	defer timer.Stop()

	var published string

	for {
		select {
		case <-r.stop:
			return
		case <-r.notify:
			// A phase is published only once it stayed current for the dwell time.
			timer.Reset(r.dwell)
		case <-timer.C:
			phase := r.current()
			if phase == "" || phase == published || r.stopped() {
				continue
			}

			err := r.post(commitstatus.WithSingleAttempt(ctx), phase)

			switch {
			case err == nil:
				published = phase
			case commitstatus.IsRateLimited(err):
				r.log.Warn("pausing deployment phase updates in the commit status: rate limited by the Git provider",
					slog.String("error", err.Error()))

				return
			case lifecycle.IsCanceled(err) || ctx.Err() != nil:
				return
			default:
				r.log.Debug("failed to post deployment phase to the commit status",
					slog.String("phase", phase), slog.String("error", err.Error()))
			}
		}
	}
}

// startPhaseReporter returns a reporter publishing the deployment's phases in
// its commit status, or nil when there is nothing to update: the deployment's
// "In Progress" status was not posted, or its provider cannot show phases.
func (s *StageManager) startPhaseReporter(ctx context.Context, dwell time.Duration) *phaseReporter {
	if !s.inProgressPosted || s.DeployConfig.Destroy.Enabled {
		return nil
	}

	params := s.commitStatusParams()

	req, ok := commitstatus.ResolveRequest(s.Log, params)
	if !ok || !req.SupportsProgress() {
		return nil
	}

	log := s.Log

	return newPhaseReporter(ctx, log, dwell, func(ctx context.Context, phase string) error {
		// Resolve per post: a long deployment can outlive the token. Cached
		// tokens make this free, and the shared Target keeps updating the
		// same check run.
		req, ok := commitstatus.ResolveRequest(log, params)
		if !ok {
			return nil
		}

		return req.Post(ctx, commitstatus.ProgressStatus(phase))
	})
}
