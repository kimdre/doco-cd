package config

// DeployConfigMeta is the deployment configuration meta data
type DeployConfigMeta struct {
	// DeploymentConfigFilePath is the default path or regex pattern to the deployment configuration file
	// in a repository and overrides the default deployment configuration
	DeploymentConfigFilePath string `env:"DEPLOYMENT_CONFIG_FILE_NAME" envDefault:"./webhook-deployment.y(a)?ml"`
}

// DeployConfigStructure is the structure of the deployment configuration file
type DeployConfigStructure struct {
	// Reference is the reference to the deployment
	// e.g. the branch refs/heads/branch_name or tag refs/tags/tag_name
	Reference string `yaml:"reference"`
	// Name is the name of the docker-compose deployment / stack
	Name string `yaml:"name"`
	// DockerComposePath is the path to the docker-compose file
	DockerComposePath string `yaml:"docker_compose_path"`
	// DockerComposeEnvFiles is the path to the environment files to use
	DockerComposeEnvFiles []string `yaml:"docker_compose_env_files"`
	// SkipTLSVerification skips the TLS verification
	SkipTLSVerification bool `yaml:"skip_tls_verify"`
}

// DefaultDeployConfig returns the default values for the deployment configuration
func DefaultDeployConfig() *DeployConfigStructure {
	return &DeployConfigStructure{
		Reference:             "/ref/heads/main",
		Name:                  "",
		DockerComposePath:     "docker-compose.y(a)?ml",
		DockerComposeEnvFiles: nil,
		SkipTLSVerification:   false,
	}
}
