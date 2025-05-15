package docker

// docoCDLabelNamesMetadata contains the metadata labels used by DocoCD
type docoCDLabelNamesMetadata struct {
	Manager string // Name of the deployment manager (e.g., DocoCD)
	Version string // Doco-CD version used at the time of deployment
}

// DocoCDLabelNamesDeployment contains the labels used by DocoCD to identify deployed containers
type docoCdLabelNamesDeployment struct {
	Name          string // Name of the deployment
	Timestamp     string // Timestamp of deployment in RFC3339 format
	WorkingDir    string // Working Directory where the deployment gets executed
	TargetRef     string // Target reference (branch/tag) of the deployment
	TriggerCommit string // SHA of the commit that triggered the deployment
}

// docoCdLabelNamesRepository contains the labels used by DocoCD to identify the repository
type docoCdLabelNamesRepository struct {
	Name string
	URL  string
}

// docoCdLabelNames contains the labels used by DocoCD to identify deployed containers and their metadata
type docoCdLabelNames struct {
	Metadata   docoCDLabelNamesMetadata   // Metadata about the deployment manager
	Deployment docoCdLabelNamesDeployment // Labels related to the deployment
	Repository docoCdLabelNamesRepository // Labels related to the repository
}

// DocoCDLabels contains the label key names used by DocoCD to identify deployed containers and their metadata
var DocoCDLabels = docoCdLabelNames{
	Metadata: docoCDLabelNamesMetadata{
		Manager: "cd.doco.metadata.manager",
		Version: "cd.doco.metadata.version",
	},
	Deployment: docoCdLabelNamesDeployment{
		Name:          "cd.doco.deployment.name",
		Timestamp:     "cd.doco.deployment.timestamp",
		WorkingDir:    "cd.doco.deployment.working_dir",
		TargetRef:     "cd.doco.deployment.target.ref",
		TriggerCommit: "cd.doco.deployment.trigger.commit",
	},
	Repository: docoCdLabelNamesRepository{
		Name: "cd.doco.repository.name",
		URL:  "cd.doco.repository.url",
	},
}
