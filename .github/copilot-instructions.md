# Copilot instructions for doco-cd

doco-cd is a GitOps continuous deployment tool for Docker Compose and Docker Swarm, written in Go. User documentation lives in `wiki/docs/` (published with Zensical). Contribution rules are in `CONTRIBUTING.md`.

## Build, test, and lint

- `make tools` installs golangci-lint and the Go tools from the `tool` block in `go.mod` into `./.bin`. Run it once before linting.
- `make fmt` (alias `make lint`) runs `.bin/golangci-lint run --fix ./...` and `go fix ./...`. Run it after every code change. CI fails on any lint finding and also runs codespell.
- `make build` builds all binaries into `bin/` with `CGO_ENABLED=0 -tags nobitwarden`. `make build-bitwarden` includes the Bitwarden SDK and needs cgo (`musl-gcc` on Linux, `clang` on macOS).
- `make test` runs the full suite with the Bitwarden SDK (cgo). `make test-nobitwarden` runs it without cgo or a C compiler.
- Run a single unit test:
  ```sh
  WEBHOOK_SECRET=test_Secret1 API_SECRET=test_apiSecret1 CGO_ENABLED=0 \
    go test -tags nobitwarden ./internal/<pkg> -run 'TestName/subtest'
  ```
  `make test-run TestName` also works, but it runs `-run` across `./...` with cgo.
- The Makefile includes and exports `.env`. Local values there can break tests that assert configuration defaults. In that case, run `go test` directly instead of through `make`.
- Many unit tests need a real Docker daemon. CI runs the unit suite twice, once in standalone mode and once after `docker swarm init`, so Docker-touching code must work in both modes. Some tests also need `GIT_ACCESS_TOKEN`, `SOPS_AGE_KEY`, or the `test/test-cases` git submodule (`git submodule update --init`).
- Reconciliation Docker event integration tests only run with `DOCO_CD_RUN_DOCKER_INTEGRATION_TESTS=1`.
- E2E tests use the `e2e` build tag and drive a real doco-cd container (see `test/e2e/README.md`):
  ```sh
  make test-e2e E2E_RUN=TestFailedDeployRetry
  # or
  go test -tags e2e ./test/e2e/... -run TestFailedDeployRetry -v
  ```
  Leave `E2E_TEST_MODE` unset locally to run all compatible scenarios, or set `standalone`/`swarm` to match a CI job. Harnesses stay up until suite teardown unless `E2E_KEEP_COMPONENTS_RUNNING=0` (as in CI). The harness builds doco-cd on the host with `-tags nobitwarden` and needs Docker with BuildKit.
- Deploy config JSON schema: `go run ./cmd/deploy-schema` writes `wiki/docs/schemas/deploy.schema.json` (gitignored). Test the generator with `go test ./cmd/deploy-schema`.
- Wiki: `make wiki-tools` then `make wiki-serve`. CI runs `zensical build --config-file wiki/zensical.toml --strict`, so broken links fail the build. Wiki hooks in `wiki/hooks/` call Go generators (deploy schema, metrics table from `cmd/metrics-docs`).

## Architecture

`cmd/doco-cd/main.go` (`run()`) wires everything in this order: app config, notifier (Apprise), Git auth resolver, Docker CLI plus `docker.ContextRegistry` (multiple Docker contexts, Swarm detection per context), data mount (`DATA_MOUNT_PATH`), legacy layout migration, self-update, secret provider, `reconciliation.Manager`, `source.Preparer`, `controlplane.Deployment`, `scheduler.Manager`, `controlplane.Runs`, the HTTP API (`internal/api`) and optional MCP server (`internal/mcp`), poll jobs, and the artifact/source GC sweepers. The `apply-self` subcommand is the self-update applier entry point.

A deployment flows through these layers:

1. **Trigger**: webhook (`cmd/doco-cd/handler_webhook.go`, `internal/webhook`), poll (`internal/controlplane/poll.go`), API/MCP, or scheduler. `controlplane.Runs` tracks every run and its lifecycle (`RunSynchronous`, `RunSynchronousDetached`, `RunAsynchronous`).
2. **Control plane** (`internal/controlplane`): `Deployment.Deploy` is protocol-neutral. Sentinel errors from lower layers map to HTTP status codes through `DeploymentError`.
3. **Source** (`internal/source`): `Preparer.Prepare` resolves a Git or OCI source into an immutable revision directory managed by `internal/source/store` and `internal/source/cache`, then loads deploy configs.
4. **Reconciliation** (`internal/reconciliation`): `Manager` handles admission, concurrency limits, sync windows, retries of failed deployments, and scheduler holds.
5. **Stages** (`internal/stages`): `StageManager.RunStages` runs Init, PreDeploy, Deploy, PostDeploy, Cleanup (or Init, Destroy, PostDestroy, Cleanup for destroy). `ErrSkipDeployment` and `selfupdate.ErrHandover` are non-failure outcomes. Commit statuses (`internal/commitstatus`) are only posted after PreDeploy confirms there is work to do.
6. **Docker** (`internal/docker`): Compose and Swarm deployers, labels, scheduled jobs, self-update. Compose and Swarm code paths are usually split into `compose_*.go` and `swarm_*.go` files.

Package layering is strict to avoid import cycles: `docker` < `stages` < `source` < `controlplane` < `api`/`mcp`. `internal/source` must not import `controlplane` or `reconciliation`. `internal/stages` cannot import `internal/gc` because `gc` imports `stages` through `source`. Inject such logic via an interface or call it from `controlplane`.

doco-cd persists deployment state on Docker objects, not in a database. Labels in `internal/docker/labels.go` (`cd.doco.deployment.*`, `cd.doco.source.*`, `cd.doco.job.*`) are the source of truth for drift detection, auto-discovery, scheduler recovery, and cleanup. `DATA_MOUNT_PATH` holds only source caches and small state files (for example scheduler finalization records).

## Conventions

- Naming: YAML/JSON keys are `snake_case`, environment variables are `UPPER_SNAKE_CASE`, Go identifiers follow Go style. revive allows the initialisms `ID`, `HTTP`, `URL`, `API`.
- Lint rules that commonly trip new code: `wsl_v5` (blank lines around blocks, assignments, and returns), `godot` (comments end with a period), `gofumpt`, and `goimports` with local prefix `github.com/kimdre/doco-cd` (project imports in their own group). Scope `gosec` suppressions to a rule and give a reason, for example `// #nosec G304: path comes from a fixed fixture list.`
- App config (`internal/config/app/app_config.go`) uses `caarlos0/env` tags such as `env:"NAME" envDefault:"..."` plus `validate:"..."`. Secrets also accept a `*_FILE` variant. Document new settings in `wiki/docs/App-Settings.md`.
- Deploy config (`internal/config/deploy/`) uses `yaml`/`json` snake_case tags, `default:"..."` tags, and `validate` tags. Fields tagged `doco:"allowOverride"` can be overridden by auto-discovery. Trailing field comments, `default` tags, and validation tags feed the JSON schema, so when you add custom unmarshaling or validation, update the rules in `cmd/deploy-schema/`.
- Validate structs with `internal/common/validation`. Tag live runtime dependencies (loggers, clients, registries) with `nostructlevel` so validation does not traverse them.
- Deep-copy structs with reference-type fields using `internal/common/types/clone` (`clone.New` or `clone.Deep`), not ad hoc copies.
- Compose lifecycle hook runners (containers with a non-empty `api.HookLabel`) inherit service and job labels but are not replicas. Exclude them when you list raw containers for service inventories or job discovery.
- Self-update overlaps the old and new doco-cd versions on the same `DATA_MOUNT_PATH`. Any change to persisted state formats must stay readable by both versions.
- Git poll URLs accept HTTP(S), SSH, `file://`, and absolute paths. Native `git://` is rejected (`git.IsNativeGit`). HTTP(S) credentials are only sent to hosts matching `GIT_AUTH_DOMAINS` unless `GIT_AUTH_ALLOW_ANY_HOST` is set.
- Tests: name Docker stacks with `test.ConvertTestName(t.Name())` so parallel tests do not collide. In E2E tests, assert on Docker state rather than log lines, use `LogMark` plus `WaitForLogAfter` for multi-phase log checks, prefix stack names with `e2e-`, call `requireStandaloneSuite(t)` for Compose-only scenarios, and keep Swarm scheduled-job recovery tests serial.
- Docs: user-visible behavior changes need matching updates in `wiki/docs/` (US English, relative links).
- Commits follow Conventional Commits and must be signed. Repository skills for commits, PR review, and change review live in `.github/skills/`.
