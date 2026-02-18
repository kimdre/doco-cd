package docker

// docoCDLabelNamesMetadata contains the metadata labels used by DocoCD.
type docoCDLabelNamesMetadata struct {
	Manager string // Name of the deployment manager (e.g., DocoCD)
	Version string // Doco-CD version used at the time of deployment
}

// DocoCDLabelNamesDeployment contains the labels used by DocoCD to identify deployed containers.
type docoCdLabelNamesDeployment struct {
	Name                string // Name of the deployment
	Timestamp           string // Timestamp of deployment in RFC3339 format
	WorkingDir          string // Working Directory where the deployment gets executed
	TargetRef           string // Target reference (branch/tag) of the deployment
	Trigger             string // Poll or SHA of the commit that triggered the deployment
	CommitSHA           string // SHA of the commit that is currently deployed
	ConfigHash          string // SHA256 hash of the deploy-config used during deployment
	ExternalSecretsHash string // SHA256 hash of the external secrets used during deployment
	AutoDiscover        string // Whether the deployment was auto-discovered
	AutoDiscoverDelete  string // Whether auto-discovered deployment is allowed to be deleted
}

// docoCdLabelNamesRepository contains the labels used by DocoCD to identify the repository.
type docoCdLabelNamesRepository struct {
	Name string
	URL  string
}

// docoCdLabelNames contains the labels used by DocoCD to identify deployed containers and their metadata.
type docoCdLabelNames struct {
	Metadata   docoCDLabelNamesMetadata   // Metadata about the deployment manager
	Deployment docoCdLabelNamesDeployment // Labels related to the deployment
	Repository docoCdLabelNamesRepository // Labels related to the repository
}

// DocoCDLabels contains the label key names used by DocoCD to identify deployed containers and their metadata.
var DocoCDLabels = docoCdLabelNames{
	Metadata: docoCDLabelNamesMetadata{
		Manager: "cd.doco.metadata.manager",
		Version: "cd.doco.metadata.version",
	},
	Deployment: docoCdLabelNamesDeployment{
		Name:                "cd.doco.deployment.name",
		Timestamp:           "cd.doco.deployment.timestamp",
		WorkingDir:          "cd.doco.deployment.working_dir",
		TargetRef:           "cd.doco.deployment.target.ref",
		CommitSHA:           "cd.doco.deployment.target.sha",
		Trigger:             "cd.doco.deployment.trigger",
		ConfigHash:          "cd.doco.deployment.config.sha",
		ExternalSecretsHash: "cd.doco.deployment.external_secrets.sha", // #nosec G101
		AutoDiscover:        "cd.doco.deployment.auto_discover",
		AutoDiscoverDelete:  "cd.doco.deployment.auto_discover.delete",
	},
	Repository: docoCdLabelNamesRepository{
		Name: "cd.doco.repository.name",
		URL:  "cd.doco.repository.url",
	},
}

var docoCDJobLabelNames = struct {
	JobSchedule string // Schedule of the job (if applicable) in cron format
	JobLastRun  string // Timestamp of the last run in RFC3339 format
	JobNextRun  string // Timestamp of the next scheduled run in RFC3339 format
}{
	JobSchedule: "cd.doco.job.schedule",
	JobLastRun:  "cd.doco.job.last_run",
	JobNextRun:  "cd.doco.job.next_run",
}
