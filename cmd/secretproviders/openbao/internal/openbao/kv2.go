package openbao

import (
	"context"
	"fmt"

	openbao "github.com/openbao/openbao/api/v2"
)

// GetSecret retrieves a secret value from the KVv2 engine in OpenBao using the provided engine name, secret ID, and key.
func GetSecret(ctx context.Context, client *openbao.Client, engineName, id, key string) (string, error) {
	secret, err := client.KVv2(engineName).Get(ctx, id)
	if err != nil {
		return "", fmt.Errorf("failed to retrieve secret with ID %s: %w", id, err)
	}

	value, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("%w: key %s not found in secret %s", ErrInvalidSecretReference, key, id)
	}

	strValue, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("value of key %s in secret %s is not a string", key, id)
	}

	return strValue, nil
}
