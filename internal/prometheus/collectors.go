package prometheus

import "github.com/prometheus/client_golang/prometheus"

var (
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
	}, []string{"repository"})
	DeploymentErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: MetricsNamespace,
		Name:      "deployment_errors_total",
		Help:      "Total number of errors during deployments",
	}, []string{"repository"})
	DeploymentDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: MetricsNamespace,
		Name:      "deployment_duration_seconds",
		Help:      "Duration of deployment operations in seconds",
		Buckets:   prometheus.DefBuckets,
	}, []string{"repository"})
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
)

func init() {
	prometheus.MustRegister(
		AppInfo,
		PollTotal, PollErrors, PollDuration,
		WebhookRequestsTotal, WebhookErrorsTotal, WebhookDuration,
		DeploymentsTotal, DeploymentErrorsTotal, DeploymentDuration,
		DeploymentsActive, DeploymentsQueued,
	)
}
