package swarm

type DeployMode string

const (
	DeployModeReplicated    DeployMode = "replicated"
	DeployModeReplicatedJob DeployMode = "replicated-job"
	DeployModeGlobal        DeployMode = "global"
	DeployModeGlobalJob     DeployMode = "global-job"
)
