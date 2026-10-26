# Security Policy

## Supported Versions

[![GitHub Release](https://img.shields.io/github/v/release/kimdre/doco-cd?include_prereleases&sort=semver&display_name=release&style=flat-square&label=Latest%20Version&color=%234CBB17)](https://github.com/kimdre/doco-cd/releases)
![GitHub Release Date](https://img.shields.io/github/release-date/kimdre/doco-cd?style=flat-square&label=Release%20Date&color=%234CBB17)

Only the latest version is supported.

Patches are only released for the latest version.

Versioning follows [Semantic Versioning](https://semver.org/), with the following format:

- Stable tags: `vX.Y.Z`
- Prerelease tags: `vX.Y.Z-rc.N`

Versions prior to `v1.0.0` can contain breaking changes anytime and are not guaranteed to be stable.

You can always find all release notes on GitHub:

- <https://github.com/kimdre/doco-cd/releases>

## Trust Model

Doco-CD deploys what your repositories describe. It talks to the Docker daemon, so the following actors are trusted
with root-equivalent access to the Docker host:

- Anyone who can push to a repository that doco-cd deploys (including its `.doco-cd.yml` and compose files).
- Anyone who holds the `WEBHOOK_SECRET` (webhook requests can point doco-cd at any remote repository).
- Anyone who holds the `API_SECRET` (REST API and MCP server).

These actors can deploy arbitrary workloads, for example privileged containers, bind mounts of the host filesystem,
or a mount of the Docker socket. Doco-CD does not try to sandbox them.

Because of this, file paths in deployment configuration are trusted and are read from the doco-cd container filesystem
without being confined to the repository. This applies to:

- `env_files` and `external_secrets_files` in the deploy config (relative paths resolve against `working_dir` and can
  use `..`, absolute paths in `env_files` are read as given and are an intended feature).
- `env_file`, `configs` and `secrets` entries with a `file` source, and `build` secrets in compose files.

Reports that only show that these actors can read files or environment variables that are reachable from the doco-cd
container are out of scope, as they can already do the same with a deployed container.

To reduce the impact of a compromised repository or secret:

- Limit who can push to repositories that doco-cd deploys and protect the deployed branches.
- Keep `WEBHOOK_SECRET` and `API_SECRET` private and rotate them if they leak.
- Keep `PASS_ENV` disabled, so the doco-cd environment is not passed to deployments.
- Mount only the files that doco-cd needs into its container.

## Reporting a Vulnerability

If you believe you have found a security vulnerability, please report it to us through coordinated disclosure.

**Please do not report security vulnerabilities through public GitHub issues, discussions, or pull requests.**

This gives us time to work with you to fix the issue before public exposure, reducing the chance that the exploit will be used before a patch is released.

You may submit the report in the following ways:

- create a [private vulnerability report](https://github.com/kimdre/doco-cd/security/advisories/new)
- email opensource[@]drechsel.xyz.

Please include as much of the information listed below as you can to help us better understand and resolve the issue:

- Impact of the issue, including how an attacker might exploit the issue
- Step-by-step instructions to reproduce the issue
- Proof-of-concept or exploit code (if possible)
- The version and/or location of the affected source code (tag/branch/commit or direct URL)
- The type of issue (e.g., buffer overflow, SQL injection, or cross-site scripting)
- Full paths of source file(s) related to the manifestation of the issue
- Any special configuration required to reproduce the issue

This information will help us triage your report more quickly.
