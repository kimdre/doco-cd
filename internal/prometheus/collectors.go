package prometheus

import (
	"github.com/prometheus/client_golang/prometheus"

	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
)

func init() {
	deployConfig.SetAutoDiscoveryCacheObserver(func(repository, result string) {
		AutoDiscoveryCacheTotal.WithLabelValues(repository, result).Inc()
	})

	prometheus.MustRegister(
		AppInfo,
		PollTotal, PollErrors, PollDuration,
		AutoDiscoveryCacheTotal,
		WebhookRequestsTotal, WebhookErrorsTotal, WebhookDuration,
		DeploymentsTotal, DeploymentErrorsTotal, DeploymentDuration, DeploymentStageDuration,
		DeploymentQueueDuration,
		SourcePreparationDuration,
		DeploymentsActive, DeploymentsQueued,
		ScheduledRunsTotal, ScheduledRunErrorsTotal, ScheduledRunSkippedTotal,
		ScheduledRunDuration, ScheduledRunsActive,
		McpRequestsTotal, McpErrorsTotal, McpRequestDuration,
	)
}

var (
	/*
		Add new collectors below this comment
		--8<-- [start:collectors] */
	AppInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: MetricsNamespace,
		Name:      "info",
		Help:      "Application information",
	},
		[]string{"version", "log_level", "start_time"},
	)
	PollTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: MetricsNamespace,
		Name:      "polls_total",
		Help:      "Number of successful polls",
	}, []string{"repository"})
	PollErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: MetricsNamespace,
		Name:      "poll_errors_total",
		Help:      "Failed polling attempts",
	}, []string{"repository"})
	PollDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: MetricsNamespace,
		Name:      "poll_duration_seconds",
		Help:      "Duration of polling operations in seconds",
		Buckets:   prometheus.DefBuckets,
	}, []string{"repository"})
	AutoDiscoveryCacheTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: MetricsNamespace,
		Name:      "auto_discovery_cache_total",
		Help:      "Auto-discovery cache lookups by result",
	}, []string{"repository", "result"})
	WebhookRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: MetricsNamespace,
		Name:      "webhook_requests_total",
		Help:      "Total number of webhook requests received",
	}, []string{"repository"})
	WebhookErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: MetricsNamespace,
		Name:      "webhook_errors_total",
		Help:      "Total number of errors in webhook processing",
	}, []string{"repository"})
	WebhookDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: MetricsNamespace,
		Name:      "webhook_duration_seconds",
		Help:      "Duration of webhook processing in seconds",
		Buckets:   prometheus.DefBuckets,
	}, []string{"repository"})
	DeploymentsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: MetricsNamespace,
		Name:      "deployments_total",
		Help:      "Total number of deployments processed",
	}, []string{"repository", "deployment", "context"})
	DeploymentErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: MetricsNamespace,
		Name:      "deployment_errors_total",
		Help:      "Total number of errors during deployments",
	}, []string{"repository", "deployment", "context"})
	DeploymentDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: MetricsNamespace,
		Name:      "deployment_duration_seconds",
		Help:      "Duration of deployment operations in seconds",
		Buckets:   prometheus.DefBuckets,
	}, []string{"repository", "deployment", "context"})
	DeploymentStageDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: MetricsNamespace,
		Name:      "deployment_stage_duration_seconds",
		Help:      "Duration of deployment stages in seconds",
		Buckets:   prometheus.DefBuckets,
	}, []string{"repository", "deployment", "context", "stage", "outcome"})
	DeploymentQueueDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: MetricsNamespace,
		Name:      "deployment_queue_duration_seconds",
		Help:      "Time deployments spend waiting for admission in seconds",
		Buckets:   prometheus.DefBuckets,
	}, []string{"repository", "outcome"})
	SourcePreparationDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: MetricsNamespace,
		Name:      "source_preparation_duration_seconds",
		Help:      "Duration of deployment source preparation in seconds",
		Buckets:   prometheus.DefBuckets,
	}, []string{"source", "outcome"})
	DeploymentsActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: MetricsNamespace,
		Name:      "deployments_active",
		Help:      "Number of currently active deployments",
	}, []string{"repository"})
	DeploymentsQueued = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: MetricsNamespace,
		Name:      "deployments_queued",
		Help:      "Number of queued deployments waiting to start",
	}, []string{"repository"})
	ScheduledRunsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: MetricsNamespace,
		Name:      "scheduled_runs_total",
		Help:      "Total number of scheduled job runs processed",
	}, []string{"context", "stack", "job", "mode", "execution_mode"})
	ScheduledRunErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: MetricsNamespace,
		Name:      "scheduled_run_errors_total",
		Help:      "Total number of failed scheduled job runs",
	}, []string{"context", "stack", "job", "mode", "execution_mode"})
	ScheduledRunSkippedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: MetricsNamespace,
		Name:      "scheduled_run_skipped_total",
		Help:      "Total number of skipped scheduled job runs",
	}, []string{"context", "stack", "job", "mode", "execution_mode", "reason"})
	ScheduledRunDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: MetricsNamespace,
		Name:      "scheduled_run_duration_seconds",
		Help:      "Duration of scheduled job runs in seconds",
		Buckets:   prometheus.DefBuckets,
	}, []string{"context", "stack", "job", "mode", "execution_mode"})
	ScheduledRunsActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: MetricsNamespace,
		Name:      "scheduled_runs_active",
		Help:      "Number of currently active scheduled job runs",
	}, []string{"context", "stack", "job", "mode", "execution_mode"})
	McpRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: MetricsNamespace,
		Name:      "mcp_requests_total",
		Help:      "Total number of dispatched MCP tool calls",
	}, []string{"tool"})
	McpErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: MetricsNamespace,
		Name:      "mcp_errors_total",
		Help:      "Total number of failed dispatched MCP tool calls",
	}, []string{"tool"})
	McpRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: MetricsNamespace,
		Name:      "mcp_request_duration_seconds",
		Help:      "Duration of dispatched MCP tool calls in seconds",
		Buckets:   prometheus.DefBuckets,
	}, []string{"tool"})
	/* --8<-- [end:collectors]
	Add new collectors above this comment */
)
