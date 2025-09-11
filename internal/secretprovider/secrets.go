package secretprovider

type SecretProvider interface {
	Provider() string
	GetSecret(id string) (string, error)
	GetSecrets(ids []string) (map[string]string, error)
	Close()
}

// BaseConfig is the base config for a secret provider.
type BaseConfig struct {
	Provider string `env:"SECRET_PROVIDER"` // Name of the secret provider (e.g., "bitwarden-sm")
}
