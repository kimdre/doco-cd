package docker

// DocoCDLabelNamesDeployment contains the labels used by DocoCD to identify deployed containers
type docoCdLabelNamesDeployment struct {
	Manager    string // Name of the deployment manager (e.g., DocoCD)
	Timestamp  string // Timestamp of deployment in RFC3339 format
	WorkingDir string // Working Directory where the deployment gets executed
	CommitSHA  string // SHA of the commit that triggered the deployment
	CommitRef  string // Reference/Branch of the commit that triggered the deployment
}

// docoCdLabelNamesRepository contains the labels used by DocoCD to identify the repository
type docoCdLabelNamesRepository struct {
	Name string
	URL  string
}

// docoCdLabelNames contains the labels used by DocoCD to identify deployed containers and their metadata
type docoCdLabelNames struct {
	Deployment docoCdLabelNamesDeployment // Labels related to the deployment
	Repository docoCdLabelNamesRepository // Labels related to the repository
}

// DocoCDLabels contains the label key names used by DocoCD to identify deployed containers and their metadata
var DocoCDLabels = docoCdLabelNames{
	Deployment: docoCdLabelNamesDeployment{
		Manager:    "cd.doco.deployment.manager",
		Timestamp:  "cd.doco.deployment.timestamp",
		WorkingDir: "cd.doco.deployment.working_dir",
		CommitSHA:  "cd.doco.deployment.commit.sha",
		CommitRef:  "cd.doco.deployment.commit.ref",
	},
	Repository: docoCdLabelNamesRepository{
		Name: "cd.doco.repository.name",
		URL:  "cd.doco.repository.url",
	},
}
