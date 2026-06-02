# Secretprovider Template

This directory contains starter boilerplate for a new secretprovider plugin under `cmd/secretproviders/`.

## Quick start

1. Copy this directory to a new provider name:
   - `cp -R cmd/secretproviders/template cmd/secretproviders/<provider-name>`
2. Replace placeholder tokens in copied files:
   - `__PROVIDER_NAME__` (folder and package name)
   - `__PROVIDER_DISPLAY_NAME__` (human-friendly provider name)
3. Implement provider-specific config and API calls in `internal/provider/`.
4. Build the provider image with `PLUGIN_NAME=<provider-name>`.

## Included files

- `Dockerfile`: symlink to `cmd/secretproviders/generic.Dockerfile`
- `main.go.tmpl`: wire-up to the shared gRPC server harness
- `internal/provider/config.go.tmpl`: env-based config loader skeleton
- `internal/provider/provider.go.tmpl`: provider implementation skeleton

## Using CGO

- If the plugin requires CGO, create an empty marker file at `cmd/secretproviders/<name>/.cgo_required`.
- If no marker file exists, the plugin is treated as pure Go (`CGO_ENABLED=0`).

## Notes

- Keep provider logic isolated to `internal/provider/`.
- Reuse shared code from `cmd/secretproviders/internal/server`.
- Keep exported env vars documented in your provider README.
