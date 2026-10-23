package openbao

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

// maxKVListDepth limits how many folder levels a recursive KVv2 listing follows.
const maxKVListDepth = 16

// GetSecretsByPath retrieves all string values of the KVv2 secret at the given path.
// The path can also be a folder. Then the secrets in it are included as well.
// With recursive set, all sub folders are followed.
// The returned names are relative to the path: "key" for the secret at the path itself
// and "sub/secret/key" for secrets below it. Values that are not strings are skipped.
func GetSecretsByPath(ctx context.Context, client *openbao.Client, engineName, path string, recursive bool) (map[string]string, error) {
	out := make(map[string]string)

	if err := collectKVSecrets(ctx, client, engineName, strings.Trim(path, "/"), "", recursive, 0, out); err != nil {
		return nil, err
	}

	return out, nil
}

// collectKVSecrets adds the secret at the path and the secrets below it to out.
// The relative argument is the part of the path below the requested path.
func collectKVSecrets(ctx context.Context, client *openbao.Client, engineName, path, relative string, recursive bool, depth int, out map[string]string) error {
	if depth > maxKVListDepth {
		return fmt.Errorf("%w: folder nesting in %s is deeper than %d levels", ErrInvalidSecretReference, engineName, maxKVListDepth)
	}

	if path != "" {
		secret, err := client.KVv2(engineName).Get(ctx, path)

		switch {
		case err == nil:
			for key, value := range secret.Data {
				strValue, ok := value.(string)
				if !ok {
					continue
				}

				// A key with a slash can have the same name as a key in a sub folder.
				name := joinRelative(relative, key)
				if _, exists := out[name]; exists {
					return fmt.Errorf("%w: more than one value has the name %q", ErrDuplicateSecretName, name)
				}

				out[name] = strValue
			}
		case !errors.Is(err, openbao.ErrSecretNotFound):
			return fmt.Errorf("failed to retrieve secret with ID %s: %w", path, err)
		}
	}

	if depth > 0 && !recursive {
		return nil
	}

	children, err := listKVChildren(ctx, client, engineName, path)
	if err != nil {
		return err
	}

	// A name can be a secret and a folder at the same time. One visit handles both.
	visited := make(map[string]struct{}, len(children))

	for _, child := range children {
		name, isFolder := strings.CutSuffix(child, "/")

		if isFolder && !recursive {
			continue
		}

		if _, done := visited[name]; done {
			continue
		}

		visited[name] = struct{}{}

		childPath := name
		if path != "" {
			childPath = path + "/" + name
		}

		if err := collectKVSecrets(ctx, client, engineName, childPath, joinRelative(relative, name), recursive, depth+1, out); err != nil {
			return err
		}
	}

	return nil
}

// listKVChildren returns the names of the entries in a KVv2 folder. Folder names end with a slash.
func listKVChildren(ctx context.Context, client *openbao.Client, engineName, path string) ([]string, error) {
	listPath := engineName + "/metadata"
	if path != "" {
		listPath += "/" + path
	}

	secret, err := client.Logical().ListWithContext(ctx, listPath)
	if err != nil {
		return nil, fmt.Errorf("failed to list secrets in %s: %w", path, err)
	}

	if secret == nil || secret.Data == nil {
		return nil, nil
	}

	raw, ok := secret.Data["keys"].([]any)
	if !ok {
		return nil, nil
	}

	children := make([]string, 0, len(raw))

	for _, entry := range raw {
		if name, ok := entry.(string); ok && name != "" {
			children = append(children, name)
		}
	}

	return children, nil
}

func joinRelative(relative, name string) string {
	if relative == "" {
		return name
	}

	return relative + "/" + name
}
