package docker

// docoCDLabelNamesMetadata contains the metadata labels used by DocoCD.
type docoCDLabelNamesMetadata struct {
	Manager string // Name of the deployment manager (e.g., DocoCD)
	Version string // Doco-CD version used at the time of deployment
}

// DocoCDLabelNamesDeployment contains the labels used by DocoCD to identify deployed containers.
type docoCdLabelNamesDeployment struct {
	Name                 string // Name of the deployment
	Timestamp            string // Timestamp of deployment in RFC3339 format
	ComposeHash          string // SHA256 hash of the generated compose project contents
	WorkingDir           string // Working Directory where the deployment gets executed
	TargetRef            string // Target reference (branch/tag) of the deployment
	Trigger              string // Poll or SHA of the commit that triggered the deployment
	CommitSHA            string // SHA of the commit that is currently deployed
	ConfigHash           string // SHA256 hash of the deploy-config used during deployment
	AutoDiscovery        string // Whether the deployment was auto-discovered
	AutoDiscoveryDelete  string // Whether auto-discovered deployment is allowed to be deleted
	RecreateIgnore       string // Whether the deployment file changes should ignore recreate
	RecreateIgnoreSignal string // Signal service when deployment file changes and ignore recreate
}

// docoCdLabelNamesRepository contains the labels used by DocoCD to identify the repository.
type docoCdLabelNamesRepository struct {
	Name string
	URL  string
}

// docoCdLabelNamesService contains labels applied directly to services.
type docoCdLabelNamesService struct {
	ExternallyManaged string
}

// docoCdLabelNames contains the labels used by DocoCD to identify deployed containers and their metadata.
type docoCdLabelNames struct {
	Metadata   docoCDLabelNamesMetadata   // Metadata about the deployment manager
	Deployment docoCdLabelNamesDeployment // Labels related to the deployment
	Repository docoCdLabelNamesRepository // Labels related to the repository
	Service    docoCdLabelNamesService    // Labels related to service-level behavior
}

// DocoCDLabels contains the label key names used by DocoCD to identify deployed containers and their metadata.
var DocoCDLabels = docoCdLabelNames{
	Metadata: docoCDLabelNamesMetadata{
		Manager: "cd.doco.metadata.manager",
		Version: "cd.doco.metadata.version",
	},
	Deployment: docoCdLabelNamesDeployment{
		Name:                 "cd.doco.deployment.name",
		Timestamp:            "cd.doco.deployment.timestamp",
		ComposeHash:          "cd.doco.deployment.compose.sha",
		WorkingDir:           "cd.doco.deployment.working_dir",
		TargetRef:            "cd.doco.deployment.target.ref",
		CommitSHA:            "cd.doco.deployment.target.sha",
		Trigger:              "cd.doco.deployment.trigger",
		ConfigHash:           "cd.doco.deployment.config.sha",
		AutoDiscovery:        "cd.doco.deployment.auto_discovery",
		AutoDiscoveryDelete:  "cd.doco.deployment.auto_discovery.delete",
		RecreateIgnore:       "cd.doco.deployment.recreate.ignore",
		RecreateIgnoreSignal: "cd.doco.deployment.recreate.ignore.signal",
	},
	Repository: docoCdLabelNamesRepository{
		Name: "cd.doco.repository.name",
		URL:  "cd.doco.repository.url",
	},
	Service: docoCdLabelNamesService{
		ExternallyManaged: "cd.doco.service.externally_managed",
	},
}

/*
DeprecatedAutoDiscoverLabel and DeprecatedAutoDiscoverDeleteLabel are the old label names
kept for backwards-compatible reads. New deployments only write the new labels.

Deprecated: Use DocoCDLabels.Deployment.AutoDiscovery and DocoCDLabels.Deployment.AutoDiscoveryDelete instead.

TODO: Remove in a future release.
*/
const (
	DeprecatedAutoDiscoverLabel       = "cd.doco.deployment.auto_discover"
	DeprecatedAutoDiscoverDeleteLabel = "cd.doco.deployment.auto_discover.delete"
)

var docoCDJobLabelNames = struct {
	JobEnabled       string // Enable scheduling for a service/container
	JobSchedule      string // Schedule of the job in 5-field cron format or @every duration
	JobSkipRunning   string // Skip a schedule trigger when a previous run is still in progress
	JobExecutionMode string // Defines if a run restarts/reruns the job or starts an ephemeral one-shot execution
	JobNotifyOn      string // Controls notification behavior: none, success, failure, all
	JobSwarmReplicas string // Number of replicas for one-shot replicated-job runs in swarm mode
	JobLastRun       string // Timestamp of the last run in RFC3339 format
	JobNextRun       string // Timestamp of the next scheduled run in RFC3339 format
}{
	JobEnabled:       "cd.doco.job.enabled",
	JobSchedule:      "cd.doco.job.schedule",
	JobSkipRunning:   "cd.doco.job.skip_running",
	JobExecutionMode: "cd.doco.job.execution_mode",
	JobNotifyOn:      "cd.doco.job.notify_on",
	JobSwarmReplicas: "cd.doco.job.swarm.replicas",
	JobLastRun:       "cd.doco.job.last_run",
	JobNextRun:       "cd.doco.job.next_run",
}

// DocoCDJobLabels exposes the scheduler/job labels for consumers outside this package.
var DocoCDJobLabels = docoCDJobLabelNames
