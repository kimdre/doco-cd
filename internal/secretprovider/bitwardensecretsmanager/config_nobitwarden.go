//go:build nobitwarden

package bitwardensecretsmanager

type Config struct {
	ApiUrl          string
	IdentityUrl     string
	AccessToken     string
	AccessTokenFile string
}

// GetConfig returns an error indicating Bitwarden is not supported in this build.
func GetConfig() (*Config, error) {
	return nil, ErrNotSupported
}
