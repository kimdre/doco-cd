package prometheus

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	deployConfig "github.com/kimdre/doco-cd/internal/config/deploy"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
)

func init() {
	deployConfig.SetAutoDiscoveryCacheObserver(func(repository, result string) {
		AutoDiscoveryCacheTotal.WithLabelValues(repository, result).Inc()
	})

	gitInternal.SetMirrorLockObserver(func(mode string, waited, held time.Duration) {
		GitMirrorLockWaitDuration.WithLabelValues(mode).Observe(waited.Seconds())
		GitMirrorLockHeldDuration.WithLabelValues(mode).Observe(held.Seconds())
	})

	prometheus.MustRegister(
		AppInfo,
		PollTotal, PollErrors, PollDuration,
		AutoDiscoveryCacheTotal,
		WebhookRequestsTotal, WebhookErrorsTotal, WebhookDuration,
		DeploymentsTotal, DeploymentErrorsTotal, DeploymentDuration, DeploymentStageDuration,
		DeploymentQueueDuration, DeploymentAdmissionDuration, PreDeployOperationDuration,
		SourcePreparationDuration,
		GitMirrorLockWaitDuration, GitMirrorLockHeldDuration,
		DeploymentsActive, DeploymentsQueued, PreDeploymentsActive, PreDeploymentsQueued,
		SyncWindowBlockedTotal,
		ScheduledRunsTotal, ScheduledRunErrorsTotal, ScheduledRunSkippedTotal,
		ScheduledRunDuration, ScheduledRunsActive,
		McpRequestsTotal, McpErrorsTotal, McpRequestDuration,
		GitMirrorPacks, GitMirrorSizeBytes, GitMirrorCompactionsTotal, GitMirrorCompactionDuration,
		ArtifactGCRemovedTotal, ArtifactGCKept,
	)

	gitInternal.SetMirrorPackObserver(func(stats gitInternal.MirrorPackStats) {
		// A mirror skipped before its packs were listed, e.g. because it was in use, reports none.
		if stats.PacksAfter >= 0 {
			gitMirrorStats.observe(stats.Repository, stats.Path, stats.PacksAfter, stats.SizeBytes)
		}

		if stats.Result == "" {
			return
		}

		GitMirrorCompactionsTotal.WithLabelValues(stats.Repository, string(stats.Mode), stats.Result).Inc()

		// Skipped and cancelled compactions did not do the work being timed.
		if stats.Result == gitInternal.MirrorCompactionCompacted || stats.Result == gitInternal.MirrorCompactionFailed {
			GitMirrorCompactionDuration.WithLabelValues(stats.Repository, string(stats.Mode)).Observe(stats.Duration.Seconds())
		}
	})
}

// DurationBuckets extends prometheus.DefBuckets up to ten minutes.
// DefBuckets end at 10s, which deployments, webhooks and source
// preparation routinely exceed, leaving their quantiles unbounded.
var DurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 120, 300, 600}

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
		Buckets:   DurationBuckets,
	}, []string{"repository"})
	AutoDiscoveryCacheTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: MetricsNamespace,
		Name:      "auto_discovery_cache_total",
		Help:      "Auto-discovery cache lookups by result (hit, miss, or bypass for scans read fully or partly from disk)",
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
		Buckets:   DurationBuckets,
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
		Buckets:   DurationBuckets,
	}, []string{"repository", "deployment", "context"})
	DeploymentStageDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: MetricsNamespace,
		Name:      "deployment_stage_duration_seconds",
		Help:      "Duration of deployment stages in seconds",
		Buckets:   DurationBuckets,
	}, []string{"repository", "deployment", "context", "stage", "outcome"})
	DeploymentQueueDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: MetricsNamespace,
		Name:      "deployment_queue_duration_seconds",
		Help:      "Time deployments spend waiting for admission in seconds",
		Buckets:   DurationBuckets,
	}, []string{"repository", "outcome"})
	PreDeployOperationDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: MetricsNamespace,
		Name:      "pre_deploy_operation_duration_seconds",
		Help:      "Duration of pre-deploy sub-operations",
		Buckets:   DurationBuckets,
	}, []string{"operation", "outcome"})
	SourcePreparationDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: MetricsNamespace,
		Name:      "source_preparation_duration_seconds",
		Help:      "Duration of deployment source preparation in seconds",
		Buckets:   DurationBuckets,
	}, []string{"source", "outcome"})
	GitMirrorLockWaitDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: MetricsNamespace,
		Name:      "git_mirror_lock_wait_seconds",
		Help:      "Time spent waiting for a bare Git mirror lock in seconds",
		Buckets:   DurationBuckets,
	}, []string{"mode"})
	GitMirrorLockHeldDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: MetricsNamespace,
		Name:      "git_mirror_lock_held_seconds",
		Help:      "Time a bare Git mirror lock was held in seconds (shared: reads, exclusive: fetches and exports)",
		Buckets:   DurationBuckets,
	}, []string{"mode"})
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
	PreDeploymentsActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: MetricsNamespace,
		Name:      "pre_deployments_active",
		Help:      "Number of active pre-deployment checks",
	}, []string{"repository"})
	PreDeploymentsQueued = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: MetricsNamespace,
		Name:      "pre_deployments_queued",
		Help:      "Number of pre-deployment checks waiting for admission",
	}, []string{"repository"})
	DeploymentAdmissionDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: MetricsNamespace,
		Name:      "deployment_admission_duration_seconds",
		Help:      "Time deployment work spends waiting for phase admission",
		Buckets:   DurationBuckets,
	}, []string{"repository", "phase", "outcome"})
	SyncWindowBlockedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: MetricsNamespace,
		Name:      "sync_window_blocked_total",
		Help:      "Total number of deployments and stack removals deferred by a sync window",
	}, []string{"repository", "deployment", "context", "window"})
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
		Buckets:   DurationBuckets,
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
	GitMirrorPacks = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: MetricsNamespace,
		Name:      "git_mirror_packs",
		Help:      "Highest number of packfiles among the bare git mirrors of a repository",
	}, []string{"repository"})
	GitMirrorSizeBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: MetricsNamespace,
		Name:      "git_mirror_size_bytes",
		Help:      "Combined size of the packfiles in all bare git mirrors of a repository in bytes",
	}, []string{"repository"})
	GitMirrorCompactionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: MetricsNamespace,
		Name:      "git_mirror_compactions_total",
		Help:      "Total number of bare git mirror packfile compactions by mode and result",
	}, []string{"repository", "mode", "result"})
	GitMirrorCompactionDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: MetricsNamespace,
		Name:      "git_mirror_compaction_duration_seconds",
		Help:      "Duration of bare git mirror packfile compactions by mode in seconds",
		Buckets:   prometheus.ExponentialBuckets(0.05, 2, 12),
	}, []string{"repository", "mode"})
	ArtifactGCRemovedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: MetricsNamespace,
		Name:      "artifact_gc_removed_total",
		Help:      "Total number of source artifacts removed by the artifact garbage collector",
	}, []string{"repository"})
	ArtifactGCKept = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: MetricsNamespace,
		Name:      "artifact_gc_kept",
		Help:      "Number of source artifacts kept by the last artifact garbage collector sweep",
	}, []string{"repository"})
	/* --8<-- [end:collectors]
	Add new collectors above this comment */
)
