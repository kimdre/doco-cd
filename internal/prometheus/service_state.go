package prometheus

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// serviceStateTimeout limits how long one scrape waits for Docker.
// It stays below the default Prometheus scrape timeout of 10 seconds,
// so a slow Docker daemon does not make the whole scrape fail.
const serviceStateTimeout = 5 * time.Second

// serviceStateCacheTTL keeps the result of one query for a short time.
// Several scrapers or a scrape burst then cause only one query to Docker.
const serviceStateCacheTTL = 5 * time.Second

// knownServiceUpdateStates lists the update states of a Swarm service.
// An empty state means the service was never updated.
var knownServiceUpdateStates = []string{
	"updating",
	"paused",
	"completed",
	"rollback_started",
	"rollback_paused",
	"rollback_completed",
}

// ServiceState is the Swarm state of one deployed service.
type ServiceState struct {
	Context      string
	Stack        string
	Service      string
	DesiredTasks uint64
	RunningTasks uint64
	// UpdateState is the Swarm update state. It is empty if the service was never updated.
	UpdateState string
}

// ServiceStateSource returns the current state of all deployed Swarm services.
type ServiceStateSource func(ctx context.Context) []ServiceState

// serviceStateCollector reads the service state at scrape time. It does not
// keep series between scrapes, so the series of removed stacks and services
// disappear on their own.
type serviceStateCollector struct {
	mu        sync.Mutex
	source    ServiceStateSource
	cached    time.Time
	desired   *prometheus.GaugeVec
	running   *prometheus.GaugeVec
	updateVec *prometheus.GaugeVec
}

var serviceStates = &serviceStateCollector{
	desired:   ServiceDesiredTasks,
	running:   ServiceRunningTasks,
	updateVec: ServiceUpdateState,
}

// SetServiceStateSource sets where the service state metrics read their data from.
// Without a source, these metrics have no series.
func SetServiceStateSource(source ServiceStateSource) {
	serviceStates.mu.Lock()
	defer serviceStates.mu.Unlock()

	serviceStates.source = source
	serviceStates.cached = time.Time{}

	serviceStates.reset()
}

// Describe implements prometheus.Collector.
func (c *serviceStateCollector) Describe(ch chan<- *prometheus.Desc) {
	c.desired.Describe(ch)
	c.running.Describe(ch)
	c.updateVec.Describe(ch)
}

// Collect implements prometheus.Collector.
func (c *serviceStateCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.source != nil && time.Since(c.cached) >= serviceStateCacheTTL {
		ctx, cancel := context.WithTimeout(context.Background(), serviceStateTimeout)
		states := c.source(ctx)

		cancel()

		c.reset()
		c.fill(states)

		c.cached = time.Now()
	}

	c.desired.Collect(ch)
	c.running.Collect(ch)
	c.updateVec.Collect(ch)
}

func (c *serviceStateCollector) reset() {
	c.desired.Reset()
	c.running.Reset()
	c.updateVec.Reset()
}

func (c *serviceStateCollector) fill(states []ServiceState) {
	for _, s := range states {
		c.desired.WithLabelValues(s.Context, s.Stack, s.Service).Set(float64(s.DesiredTasks))
		c.running.WithLabelValues(s.Context, s.Stack, s.Service).Set(float64(s.RunningTasks))

		known := false

		for _, state := range knownServiceUpdateStates {
			value := 0.0

			if state == s.UpdateState {
				value = 1
				known = true
			}

			c.updateVec.WithLabelValues(s.Context, s.Stack, s.Service, state).Set(value)
		}

		// Docker can add states later. Report them instead of dropping them.
		if !known && s.UpdateState != "" {
			c.updateVec.WithLabelValues(s.Context, s.Stack, s.Service, s.UpdateState).Set(1)
		}
	}
}
