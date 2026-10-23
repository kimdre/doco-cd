package secretprovider

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

// ResolveOptions controls how ResolveExternalSecrets interpolates values.
type ResolveOptions struct {
	// InterpolateRefs expands variables in secret references and secret paths.
	InterpolateRefs bool
	// InterpolateResolved expands references to other resolved secrets inside the secret values.
	InterpolateResolved bool
}

// ResolvedExternalSecrets is the result of ResolveExternalSecrets.
type ResolvedExternalSecrets struct {
	// Refs holds the explicit secret references after interpolation.
	Refs map[string]secrettypes.ExternalSecretRef
	// Values maps variable names to secret values.
	Values secrettypes.ResolvedSecrets
}

// ResolveExternalSecrets fetches the secrets of a deployment.
// Secrets below the given paths are fetched first. Their names are converted to variable names.
// The explicit references are resolved afterwards and win over a path secret with the same variable name.
func ResolveExternalSecrets(
	ctx context.Context,
	provider SecretProvider,
	refs map[string]secrettypes.ExternalSecretRef,
	paths []secrettypes.ExternalSecretsPath,
	opts ResolveOptions,
	log *slog.Logger,
) (ResolvedExternalSecrets, error) {
	if log == nil {
		log = slog.Default()
	}

	interpolatedRefs, err := secrettypes.InterpolateExternalSecretRefs(refs, opts.InterpolateRefs)
	if err != nil {
		return ResolvedExternalSecrets{}, fmt.Errorf("failed to interpolate external secret references: %w", err)
	}

	interpolatedPaths, err := secrettypes.InterpolateExternalSecretsPaths(paths, opts.InterpolateRefs)
	if err != nil {
		return ResolvedExternalSecrets{}, fmt.Errorf("failed to interpolate external secrets paths: %w", err)
	}

	values := make(map[string]string)

	if err = resolvePaths(ctx, provider, interpolatedPaths, values, log); err != nil {
		return ResolvedExternalSecrets{}, err
	}

	if len(interpolatedRefs) > 0 {
		encodedSecrets, err := secrettypes.EncodeExternalSecretRefs(interpolatedRefs)
		if err != nil {
			return ResolvedExternalSecrets{}, fmt.Errorf("failed to encode external secret references: %w", err)
		}

		explicit, err := provider.ResolveSecretReferences(ctx, encodedSecrets)
		if err != nil {
			return ResolvedExternalSecrets{}, fmt.Errorf("failed to resolve external secrets: %w", err)
		}

		maps.Copy(values, explicit)
	}

	values, err = secrettypes.InterpolateResolvedSecrets(values, opts.InterpolateResolved)
	if err != nil {
		return ResolvedExternalSecrets{}, fmt.Errorf("failed to interpolate resolved secrets: %w", err)
	}

	return ResolvedExternalSecrets{Refs: interpolatedRefs, Values: values}, nil
}

// resolvePaths fetches the secrets of all paths and adds them to dst.
func resolvePaths(ctx context.Context, provider SecretProvider, paths []secrettypes.ExternalSecretsPath, dst map[string]string, log *slog.Logger) error {
	if len(paths) == 0 {
		return nil
	}

	bulk, ok := provider.(BulkSecretProvider)
	if !ok {
		return fmt.Errorf("%w: %s", secrettypes.ErrBulkNotSupported, provider.Name())
	}

	for _, entry := range paths {
		found, err := bulk.GetSecretsByPath(ctx, entry.Path, entry.Recursive)
		if err != nil {
			return fmt.Errorf("failed to fetch secrets from path %q: %w", entry.Path, err)
		}

		if len(found) == 0 {
			log.Warn("no secrets found in external secrets path", slog.String("path", entry.Path))

			continue
		}

		fromPath := make(map[string]string, len(found))
		if err = secrettypes.MergeBulkSecrets(fromPath, entry, found); err != nil {
			return err
		}

		// Names are logged. Values are never logged.
		log.Debug("fetched secrets from external secrets path",
			slog.String("path", entry.Path),
			slog.Any("variables", slices.Sorted(maps.Keys(fromPath))),
		)

		for name, value := range fromPath {
			if _, exists := dst[name]; exists {
				return fmt.Errorf("path %q creates the variable %q, which another path already created", entry.Path, name)
			}

			dst[name] = value
		}
	}

	return nil
}
