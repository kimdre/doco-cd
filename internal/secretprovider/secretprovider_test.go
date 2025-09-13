package secretprovider

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/config"
)

func TestInitialize(t *testing.T) {
	ctx := t.Context()

	c, err := config.GetAppConfig()
	if err != nil {
		t.Fatal(err)
	}

	secretProvider, err := Initialize(ctx, c.SecretProvider, "v0.0.0-test")
	if err != nil {
		t.Fatalf("failed to initialize secret provider: %s", err.Error())

		return
	}

	if secretProvider != nil {
		t.Cleanup(func() {
			secretProvider.Close()
		})
	}
}
