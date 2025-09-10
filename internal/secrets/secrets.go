package secrets

type SecretProvider interface {
	GetSecret(id string) (string, error)
	GetSecrets(ids []string) (map[string]string, error)
	Close()
}
