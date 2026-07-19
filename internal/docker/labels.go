package docker

import "strings"

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
	ConfigTarget         string // Deployment config target suffix (e.g., "nas" for .doco-cd.nas.yml)
	TargetRef            string // Target reference (branch/tag) of the deployment
	Trigger              string // Poll or SHA of the commit that triggered the deployment
	CommitSHA            string // SHA of the commit that is currently deployed
	ConfigHash           string // SHA256 hash of the deploy-config used during deployment
	AutoDiscovery        string // Whether the deployment was auto-discovered
	AutoDiscoveryConfig  string // JSON-serialized AutoDiscoveryConfig settings
	RecreateIgnore       string // Whether the deployment file changes should ignore recreate
	RecreateIgnoreSignal string // Signal service when deployment file changes and ignore recreate
}

// docoCdLabelNamesSource contains the labels used by DocoCD to identify the deployment source.
type docoCdLabelNamesSource struct {
	Type string // Source type (git or oci)
	Name string // Repository or artifact name
	URL  string // Repository or artifact URL
}

// docoCdLabelNames contains the labels used by DocoCD to identify deployed containers and their metadata.
type docoCdLabelNames struct {
	Metadata   docoCDLabelNamesMetadata   // Metadata about the deployment manager
	Deployment docoCdLabelNamesDeployment // Labels related to the deployment
	Source     docoCdLabelNamesSource     // Labels related to the deployment source
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
		ConfigTarget:         "cd.doco.deployment.config.target",
		TargetRef:            "cd.doco.deployment.target.ref",
		CommitSHA:            "cd.doco.deployment.target.sha",
		Trigger:              "cd.doco.deployment.trigger",
		ConfigHash:           "cd.doco.deployment.config.sha",
		AutoDiscovery:        "cd.doco.deployment.auto_discovery",
		AutoDiscoveryConfig:  "cd.doco.deployment.auto_discovery.config",
		RecreateIgnore:       "cd.doco.deployment.recreate.ignore",
		RecreateIgnoreSignal: "cd.doco.deployment.recreate.ignore.signal",
	},
	Source: docoCdLabelNamesSource{
		Type: "cd.doco.source",
		Name: "cd.doco.source.name",
		URL:  "cd.doco.source.url",
	},
}

/*
DeprecatedAutoDiscoverLabel and DeprecatedAutoDiscoverDeleteLabel are the old label names
kept for backwards-compatible reads. New deployments only write the new labels.

Deprecated: Use DocoCDLabels.Deployment.AutoDiscovery and DocoCDLabels.Deployment.AutoDiscoveryConfig instead.

TODO: Remove in a future release.
*/
const (
	DeprecatedAutoDiscoverLabel       = "cd.doco.deployment.auto_discover"
	DeprecatedAutoDiscoverDeleteLabel = "cd.doco.deployment.auto_discover.delete"
	// DeprecatedAutoDiscoveryDeleteLabel is the pre-consolidation scalar label for the delete setting.
	DeprecatedAutoDiscoveryDeleteLabel = "cd.doco.deployment.auto_discovery.delete" //nolint:staticcheck
)

var docoCDJobLabelNames = struct {
	JobEnabled       string // Enable scheduling for a service/container
	JobSchedule      string // Schedule of the job in 5-field cron format or @every duration
	JobWaitRunning   string // Override if deployment waits for this running job based on wait_running_jobs
	JobSkipRunning   string // Skip a schedule trigger when a previous run is still in progress
	JobExecutionMode string // Defines if a run restarts/reruns the job or starts an ephemeral one-off execution
	JobEphemeral     string // Marks a runtime-created scheduler one-off target that should be ignored as drift
	JobNotifyOn      string // Controls notification behavior: none, success, failure, all
	JobSwarmReplicas string // Number of replicas for one-off replicated-job runs in swarm mode
	JobLastRun       string // Timestamp of the last run in RFC3339 format
	JobNextRun       string // Timestamp of the next scheduled run in RFC3339 format
}{
	JobEnabled:       "cd.doco.job.enabled",
	JobSchedule:      "cd.doco.job.schedule",
	JobWaitRunning:   "cd.doco.job.wait_running_jobs",
	JobSkipRunning:   "cd.doco.job.skip_running",
	JobExecutionMode: "cd.doco.job.execution_mode",
	JobEphemeral:     "cd.doco.job.ephemeral",
	JobNotifyOn:      "cd.doco.job.notify_on",
	JobSwarmReplicas: "cd.doco.job.swarm.replicas",
	JobLastRun:       "cd.doco.job.last_run",
	JobNextRun:       "cd.doco.job.next_run",
}

// DocoCDJobLabels exposes the scheduler/job labels for consumers outside this package.
var DocoCDJobLabels = docoCDJobLabelNames

// ExtractOciArtifactTag extracts the tag from OCI artifact references (e.g., "main" from "ghcr.io/kimdre/doco-cd_tests:main").
// For Git references (e.g., "refs/heads/main", "feat/app/this", "v1.0.0-rc.1", "my/app"), it returns them as-is.
// For OCI artifact references with explicit tags, it returns the tag portion after the colon.
// For OCI artifact references with digests, it extracts the digest hash.
// If the reference has no tag or digest, it returns the original reference as-is (treated as Git reference).
func ExtractOciArtifactTag(reference string) string {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return ""
	}

	// If it starts with "refs/" it's a git reference, keep it as-is
	if strings.HasPrefix(reference, "refs/") {
		return reference
	}

	// Look for @ (digest) or : (tag) separators
	atIdx := strings.LastIndex(reference, "@")
	colonIdx := strings.LastIndex(reference, ":")
	lastSlash := strings.LastIndex(reference, "/")

	// Only extract if there's an explicit tag (":") or digest ("@")
	// Otherwise treat as a git reference and keep as-is
	// (e.g., "my/app" could be Docker Hub without tag OR a git branch - keep as-is)

	// Check for tag case: registry/repo:tag (has ":" somewhere)
	if colonIdx >= 0 && colonIdx > lastSlash {
		// Tag is after the last "/" - this is an OCI artifact tag
		return reference[colonIdx+1:]
	}

	// Check for digest case: registry/repo@sha256:hash (has "@" somewhere)
	if atIdx >= 0 {
		// Extract digest portion (after @)
		digestPart := reference[atIdx+1:]
		// Digest format is algorithm:hash, we want just the hash
		if _, after, ok := strings.Cut(digestPart, ":"); ok {
			return after
		}

		return digestPart
	}

	// No explicit tag or digest - treat as git reference and keep as-is
	// This includes: "main", "v1.0.0-rc.1", "feat/app", "feat/app/this", "my/app", etc.
	return reference
}

// SourceTypeLabelValue resolves a stable label value ("git" or "oci") from a primary source,
// with a fallback (e.g. deploy config source) when the primary is empty or unknown.
func SourceTypeLabelValue(primary, fallback string) string {
	normalize := func(v string) string {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "oci":
			return "oci"
		case "git":
			return "git"
		default:
			return ""
		}
	}

	if p := normalize(primary); p != "" {
		return p
	}

	if f := normalize(fallback); f != "" {
		return f
	}

	return "git"
}
