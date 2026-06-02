# Contributing to Doco-CD

Thank you for your support. Help is always appreciated!

## Have an issue, idea or question?

- Ask questions or discuss ideas on [GitHub Discussions](https://github.com/kimdre/doco-cd/discussions)
- Report bugs or suggest features by [opening an issue](https://github.com/kimdre/doco-cd/issues/new/choose)

## Want to contribute your code?

### Required tools

The following tools need to be installed:

- A IDE/Editor of your choice
- [Git](https://git-scm.com/)
- Golang (see the [`go.mod`](https://github.com/kimdre/doco-cd/blob/main/go.mod#L3) file for the currently used version.
- [Make](https://www.gnu.org/software/make/)
- [buf](https://buf.build/docs/installation/) (required when editing `.proto` files; see [Working with gRPC definitions](#working-with-grpc-definitions))
- `protoc-gen-go` and `protoc-gen-go-grpc` on `$PATH` (`go install google.golang.org/protobuf/cmd/protoc-gen-go@latest` and `go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest`)

### Additional steps

Follow the installation instructions for the following dependencies (required only when building or testing CGO-dependent secret-provider plugins):

- [Bitwarden Go SDK](https://github.com/bitwarden/sdk-go/tree/main?tab=readme-ov-file#installation) for `cmd/secretproviders/bitwardensecretsmanager`
- A C toolchain (`musl-gcc` on Linux, `clang` on macOS) for `cmd/secretproviders/1password` and `cmd/secretproviders/bitwardensecretsmanager`

### Getting started

1. Set up [Git Commit Signing](https://docs.github.com/en/authentication/managing-commit-signature-verification/signing-commits) if you haven't already done so. Please always sign your commits!
2. Clone the repository.
3. In the cloned repo, execute `make tools` on your command line to download/install the Go tool binaries (like linters and formatters).

### Coding style

Please follow these guidelines when contributing code:

- Write comments and documentation where necessary.
- Follow Go best practices and conventions. Refer to the [Effective Go](https://golang.org/doc/effective_go) guide for more information.
- Write unit tests for your code that cover various scenarios and edge cases including error cases if applicable.
- YAML and JSON fields must be in `snake_case`, Go struct fields must be in `camelCase`, and environment variables must be in `UPPER_SNAKE_CASE`.
- Write clear, concise, and descriptive commit messages and use [conventional commits](https://www.conventionalcommits.org/en/v1.0.0/).
- Always sign your commits. See [Signing commits](https://docs.github.com/en/authentication/managing-commit-signature-verification/signing-commits) for more information.

### Testing

After you wrote your code, run `make fmt` to format and lint your code. Adjust your code accordingly to the proposals of the linter output. 
The CI pipeline will fail, if the code is not formatted correctly.

Always add unit tests to verify your code. 
Run the tests with `make test` or `make test-verbose` and run specific tests with `make test-run <testName>` (Verbose with `make test-run "-v <testName>"`).

#### With registry credentials

You can provide container registry credentials to avoid rate limiting issues when running tests.

Follow the [Accessing private container registries](https://doco.cd/latest/Tips-and-Tricks/#accessing-private-container-registries) Guide in the docs for more information.

### Building from Source

Create a `.env` file and add any environment variables required for your development environment.

For example

```dotenv
GIT_ACCESS_TOKEN=xxx
WEBHOOK_SECRET=test_secret
SOPS_AGE_KEY=AGE-SECRET-KEY-xxx
APPRISE_NOTIFY_URLS=xxx
API_SECRET=xxx
```

Run the following command to build and run the doco-cd dev container:

```bash
docker compose -f dev.compose.yaml up --build
```

#### Building container images

The core `doco-cd` image cross-compiles via [`tonistiigi/xx`](https://github.com/tonistiigi/xx) with `CGO_ENABLED=0`:

```bash
make docker-build
```

Each plugin has its own `Dockerfile` under `cmd/secretproviders/<name>/`. Build them all (matrix over `SECRET_PROVIDER_PLUGINS`) with:

```bash
make docker-build-plugins
```

Override `DOCKER_PLATFORMS` (default: `linux/$(go env GOARCH)`) to cross-build:

```bash
make docker-build-plugins DOCKER_PLATFORMS=linux/amd64,linux/arm64
```

### Secret-provider plugins

Secret providers run as out-of-process gRPC plugins. The `doco-cd` core is plugin-agnostic: it only dials the gRPC endpoint defined by [`api/secretprovider/v1/secretprovider.proto`](api/secretprovider/v1/secretprovider.proto) and never names a specific backend.

#### Layout

```
api/secretprovider/v1/                          # proto contract + generated Go stubs
  ├── secretprovider.proto                      # gRPC contract (edit this)
  └── *.pb.go                                   # generated stubs (do not edit)
internal/secretprovider/grpc/                   # client used by doco-cd core
cmd/secretproviders/internal/server/            # shared gRPC server harness
cmd/secretproviders/.template/                  # scaffold for new plugins
  ├── Dockerfile -> ../generic.Dockerfile       # symlink to shared plugin Dockerfile
  ├── main.go.tmpl                              # entrypoint boilerplate
  └── internal/provider/*.go.tmpl               # provider/config boilerplate
cmd/secretproviders/<name>/                     # one directory per plugin
  ├── Dockerfile                                # cross-built via tonistiigi/xx, usually a symlink to `../generic.Dockerfile`
  ├── main.go                                   # wires the provider to the harness
  └── internal/<name>/                          # provider implementation
```

#### Working with gRPC definitions

[`buf`](https://buf.build) manages the protobuf definitions.

| Target                | Action                                                                  |
|-----------------------|-------------------------------------------------------------------------|
| `make buf-lint`       | Lint `.proto` files against the STANDARD rule set.                      |
| `make buf-format`     | Format `.proto` files in place.                                         |
| `make buf-generate`   | Regenerate Go stubs in `api/secretprovider/v1/`. Commit the diff.       |
| `make buf-breaking`   | Compare current `.proto` against `main` for breaking changes.           |
| `make buf`            | Convenience target running `buf-lint` + `buf-generate`.                 |

The generator config lives in [`buf.yaml`](buf.yaml) and [`buf.gen.yaml`](buf.gen.yaml).

After editing `.proto` files: `make buf-generate` and commit the regenerated `api/secretprovider/v1/*.pb.go` together with the proto change.

#### Adding a new plugin

1. **Scaffold** `cmd/secretproviders/<name>/` from the template:

    ```bash
    cp -R cmd/secretproviders/.template cmd/secretproviders/<name>
    ```

    Then rename template files and placeholders:

    - `main.go.tmpl` -> `main.go`
    - `internal/provider/config.go.tmpl` and `internal/provider/provider.go.tmpl` -> your provider package files
    - Replace `__PROVIDER_NAME__` and `__PROVIDER_DISPLAY_NAME__`

2. **Implement provider logic:**

    - Move/adapt files under `internal/provider/` to `internal/<name>/`
    - Provider implementation must satisfy `server.Provider` (`Name`, `GetSecrets`, `ResolveSecretReferences`, `Close`)
    - `main.go` should load config, build the provider, and call `server.Run(Version, getProvider)`

3. **Dockerfile convention:**

    - Keep `cmd/secretproviders/<name>/Dockerfile` as a symlink to `../generic.Dockerfile` unless plugin-specific image dependencies require a custom Dockerfile.

4. **Mark CGO requirement (only if needed):**

    - If the plugin requires CGO, create an empty marker file at `cmd/secretproviders/<name>/.cgo_required`.
    - If no marker file exists, the plugin is treated as pure Go (`CGO_ENABLED=0`).

5. **Plugin discovery:**

    - Plugin directories under `cmd/secretproviders/` are auto-discovered by Makefile and CI.
    - Plugin image workflows discover directories under `cmd/secretproviders/` (excluding `internal` and `.template`) and pass `PLUGIN_NAME` as a build arg from the directory name.

6. **Document the plugin** under `wiki/docs/External-Secrets/<Name>.md` and link it from `wiki/docs/External-Secrets/index.md`.

7. **Run** `make test`, `make build`, and `make docker-build-plugin-<name>` locally before opening the PR.

The core's `SECRET_PROVIDER` accepts only `grpc` and `webhook`. **Do not** add backend-specific names to `internal/secretprovider/secretprovider.go`.

### Documentation (Wiki)

The documentation site lives in the `wiki/` directory and is built with [Zensical](https://zensical.org/).

See the [Documentation Guidelines](wiki/README.md) for instructions on how to contribute to the documentation and run the local docs server.

### Submitting your code

1. Commit your changes with a descriptive commit message, following the [conventional commits](https://www.conventionalcommits.org/en/v1.0.0/) specification.
2. Push your changes to your fork/branch.
3. Open a pull request against the `main` branch of this repository and provide a clear description of your changes.
