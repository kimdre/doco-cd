package secretprovider_test

import (
	"errors"
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/secretprovider/bitwardensecretsmanager"
)

func TestInitialize(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	secretProvider, err := secretprovider.Initialize(ctx, c.SecretProvider, "v0.0.0-test")
	if err != nil {
		if errors.Is(err, bitwardensecretsmanager.ErrNotSupported) {
			t.Skip(err.Error())
		}

		return
	}

	if secretProvider != nil {
		t.Cleanup(func() {
			secretProvider.Close()
		})
	}
}
