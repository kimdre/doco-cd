---
tags:
  - Configuration
---

# Git Settings

Settings to configure Git authentication and clone behavior.

## General

| Key                               | Type    | Description                                                                                                                                                                                                                                                                                                                                        | Default                                          |
|-----------------------------------|---------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------|
| `GIT_CLONE_DEPTH`                 | number  | Limits the number of commits fetched during clone/fetch operations (shallow clone). `0` means full clone (no depth limit). Can be overridden per deployment via the [`git_depth`](Deploy-Settings.md) setting. When a requested ref is outside the shallow depth, doco-cd automatically deepens incrementally before falling back to a full fetch. | `0`                                              |
| `GIT_CLONE_SUBMODULES`            | boolean | Whether Git submodules are cloned too.                                                                                                                                                                                                                                                                                                             | `true`                                           |
| `SKIP_TLS_VERIFICATION`           | boolean | Skip TLS verification when cloning repositories.                                                                                                                                                                                                                                                                                                   | `false`                                          |

!!! info "Submodule URL format"
    Relative submodule URLs (for example `../other-repo.git`) are resolved against the parent repository remote URL.

    If you use an older doco-cd version and see an error like `exec: "git": executable file not found in $PATH` during submodule updates, use absolute submodule URLs (`https://...` or `git@...`) as a workaround or update to a newer version.

## Authentication

The following settings configure how Doco-CD authenticates with Git providers when cloning or pulling repositories.

Supported authentication methods:

- HTTP(S) authentication with access tokens. See [Required Token Permissions](#required-token-permissions) below.
- SSH authentication with private keys.
- If you need different credentials for different hosts, use [Domain-scoped Authentication](#domain-scoped-authentication).

| Key                               | Type   | Description                                                                                                                                                                                                                                                                             | Default                                          |
|-----------------------------------|--------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------|
| `GIT_ACCESS_TOKEN`                | string | Access token for cloning repositories (required for private repositories) via **HTTP**. See [Setup Access Token](Setup-Access-Token.md), [Required Token Permissions](#required-token-permissions), and [Domain-scoped Authentication](#domain-scoped-authentication).                  | Optional for public repositories but recommended |
| `GIT_ACCESS_TOKEN_USER`           | string | Username paired with `GIT_ACCESS_TOKEN` for **HTTP(S)** clone/fetch. Most providers accept any non-empty value, but some require a specific username for their token (e.g. a GitLab **deploy token** uses `gitlab+deploy-token-1234567`). Set it when your provider needs one.          | `oauth2`                                         |
| `GIT_ACCESS_TOKEN_FILE`           | string | Path to the file containing the Git Access Token (mutually exclusive with `GIT_ACCESS_TOKEN`).                                                                                                                                                                                          |                                                  |
| `GIT_AUTH_DOMAINS`                | list   | YAML list of domain-scoped Git credentials (HTTP token, SSH key, and GitHub App credentials). Supports exact domains and wildcard subdomains like `*.example.com` (see [Domain-scoped authentication](#domain-scoped-authentication)). Mutually exclusive with `GIT_AUTH_DOMAINS_FILE`. |                                                  |
| `GIT_AUTH_DOMAINS_FILE`           | string | Path to a file containing the YAML configuration for `GIT_AUTH_DOMAINS` (mutually exclusive with `GIT_AUTH_DOMAINS`).                                                                                                                                                                   |                                                  |
| `SSH_PRIVATE_KEY`                 | string | The private key used for cloning repositories via SSH. See [Setup SSH Key](Setup-SSH-Key.md) and [Domain-scoped Authentication](#domain-scoped-authentication).                                                                                                                         |                                                  |
| `SSH_PRIVATE_KEY_FILE`            | string | Path to the file containing the SSH private key.                                                                                                                                                                                                                                        |                                                  |
| `SSH_PRIVATE_KEY_PASSPHRASE`      | string | Passphrase for the SSH private key (if the key was generated with a passphrase).                                                                                                                                                                                                        |                                                  |
| `SSH_PRIVATE_KEY_PASSPHRASE_FILE` | string | Path to the file containing the SSH private key passphrase.                                                                                                                                                                                                                             |                                                  |

### Required Token Permissions

The token used for HTTP(S) clone/fetch access needs permission to **read repository contents**. The same requirements apply whether you use the global `GIT_ACCESS_TOKEN` or a domain-scoped token from `GIT_AUTH_DOMAINS`.

See below provider-specific setup steps:

--8<-- "wiki/includes/git-access-token-permissions.md"

### Domain-scoped Authentication

Use domain-scoped config when you fetch from multiple Git providers/domains and need separate credentials.

#### Syntax and Format

The domain-scoped authentication configuration is a YAML list where each entry defines credentials for one or more domains.

##### Entry Structure

Each entry in the list has the following structure:

```yaml
- domains:                          # (Required) List of domain names or patterns
    - domain1.com
    - domain2.com
    - '*.example.com'
  git_access_token: xxx             # (Optional) HTTP token for git access
  git_access_token_user: oauth2     # (Optional) Username for git_access_token (e.g. gitlab+deploy-token-1234567)
  ssh_private_key: |                # (Optional) SSH private key content
    -----BEGIN OPENSSH PRIVATE KEY-----
    ...
    -----END OPENSSH PRIVATE KEY-----
  ssh_private_key_passphrase: xxx   # (Optional) Passphrase for encrypted SSH key
```

##### Available Options

| Field                        | Type   | Required | Description                                                                                                                                                                                       |
|------------------------------|--------|----------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `domains`                    | list   | Yes      | List of domain names to apply these credentials to. Supports exact domains and wildcard patterns.                                                                                                 |
| `git_access_token`           | string | No       | HTTP(S) access token for authenticating with the Git provider. Cannot be used with `ssh_private_key`.                                                                                             |
| `git_access_token_user`      | string | No       | Username paired with `git_access_token`. Defaults to `oauth2`. Set it when your provider needs a specific username for the token (e.g. a GitLab deploy token uses `gitlab+deploy-token-1234567`). |
| `ssh_private_key`            | string | No       | SSH private key content (multi-line). Cannot be used with `git_access_token`.                                                                                                                     |
| `ssh_private_key_passphrase` | string | No       | Passphrase for the SSH private key if it was generated with encryption. Only used with `ssh_private_key`.                                                                                         |
| `github_app_id`              | string | No       | GitHub App ID. Requires `github_app_private_key`. Cannot be used with `git_access_token` or `ssh_private_key`.                                                                                    |
| `github_app_private_key`     | string | No       | GitHub App private key (PEM). Requires `github_app_id`. Cannot be used with `git_access_token` or `ssh_private_key`.                                                                              |
| `github_app_installation_id` | number | No       | Optional installation ID override for this domain entry. If omitted, installation is auto-detected by owner/repo.                                                                                 |

##### Authentication Method Selection

- **Use `git_access_token`** for HTTP(S) based Git access
- **Use `ssh_private_key`** (and optionally `ssh_private_key_passphrase`) for SSH-based Git access
- **Use `github_app_id` + `github_app_private_key`** for GitHub App based HTTP(S) access
- Do not mix methods in the same entry

#### Matching Behavior

- Exact domain match wins over wildcard entries.
- If multiple wildcard patterns match, the longest suffix wins (most specific).
- Wildcards only match subdomains. Example: `*.example.com` matches `git.example.com`, but not `example.com`.
- If no domain entry matches, doco-cd falls back to global `GIT_ACCESS_TOKEN` / `SSH_PRIVATE_KEY` values if set.
- Submodule remotes are resolved independently, so each submodule can use credentials for its own domain.

#### Examples

=== "Using `GIT_AUTH_DOMAINS`"

    ```yaml title="docker-compose.yml"
    services:
      app:
        environment:
          GIT_AUTH_DOMAINS: |
            --8<-- "wiki/includes/git-auth-domains.example.yaml"
    ```

=== "Using `GIT_AUTH_DOMAINS_FILE`"

    You can also store the YAML in a file and load it with `GIT_AUTH_DOMAINS_FILE`.

    ```yaml title="git-auth-domains.yaml"
    --8<-- "wiki/includes/git-auth-domains.example.yaml"
    ```
    
    ```yaml title="docker-compose.yml"
    services:
      app:
        environment:
          GIT_AUTH_DOMAINS_FILE: /run/secrets/git_auth_domains
        secrets:
          - git_auth_domains
    
    secrets:
      git_auth_domains:
        file: ./git-auth-domains.yaml
    ```

### GitHub Apps

[GitHub Apps](https://docs.github.com/en/apps) are supported natively and can be configured globally (see below) or [per domain](#domain-scoped-authentication). 
Doco-CD will auto-detect the installation by repository _owner/name_ and mint short-lived installation access tokens.

| Key                             | Type   | Description                                                                                                                                                                  | Default value     |
|---------------------------------|--------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------|
| `GITHUB_APP_ID`                 | string | ID of the GitHub App, used to mint installation access tokens for GitHub repositories. Requires `GITHUB_APP_PRIVATE_KEY`. Mutually exclusive with global `GIT_ACCESS_TOKEN`. |                   |
| `GITHUB_APP_ID_FILE`            | string | Path to the file containing `GITHUB_APP_ID` (mutually exclusive with `GITHUB_APP_ID`).                                                                                       |                   |
| `GITHUB_APP_PRIVATE_KEY`        | string | GitHub App private key in PEM format. Requires `GITHUB_APP_ID`.                                                                                                              |                   |
| `GITHUB_APP_PRIVATE_KEY_FILE`   | string | Path to the file containing `GITHUB_APP_PRIVATE_KEY` (mutually exclusive with `GITHUB_APP_PRIVATE_KEY`).                                                                     |                   |
| `GITHUB_APP_INSTALLATION_ID`    | number | Optional installation ID override for global GitHub App auth. If unset, doco-cd resolves installation by _owner/repository_ automatically.                                   | `0` (auto-detect) |

## Commit Status Reporting

Doco-CD can post a commit status back to the source Git provider after each deployment, making the result visible directly on the commit or pull request in the Git web UI.

This closes the GitOps feedback loop: instead of only seeing success or failure in container logs or Apprise notifications, the commit itself is marked with the deployment outcome.

Once a webhook's deployment configuration is resolved, Doco-CD reports each deployment under its own context, such as `doco-cd/<target>/<project>`:

- **pending / Queued**: the deployment is waiting to run.
- **pending / In Progress**: the deployment has started.
- **success**: set when all deployment stages complete successfully.
- **success / Skipped**: set when the deployment intentionally performs no work.
- **failure**: set when any stage fails after initialization.

The generic `doco-cd/deploy` context is reserved for failures that happen before deployment configuration can be resolved.

| Key                 | Type    | Description                                                                                                                                                                                                                                                                                                                                                                                                                                           | Default |
|---------------------|---------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------|
| `GIT_COMMIT_STATUS` | boolean | Enable commit status reporting. When `true`, doco-cd posts a status to the source provider for every deployment. Requires [`GIT_ACCESS_TOKEN`](#authentication) (or [domain-scoped token](#domain-scoped-authentication) via `GIT_AUTH_DOMAINS`), or a configured [GitHub App](#github-apps) with the **Commit statuses: Read and write** permission. Doco-cd reuses the same installation token minted for git operations, no separate token needed. | `false` |
| `GIT_SCM_PROVIDER`  | string  | Override automatic SCM provider detection. Accepted values: `auto`, `github`, `gitlab`, `gitea`, `forgejo`, `azuredevops`. Set to `auto` to detect the provider from the repository URL. Required when your self-hosted instance hostname does not reveal the product (e.g. `git.mycompany.com` running GitLab must set `gitlab`).                                                                                                                    | `auto`  |
| `GIT_SCM_API_URL`   | string  | Optional override for the SCM API base URL used by commit status requests (must be `http://` or `https://`). Use this for self-hosted instances when the API endpoint cannot be inferred from the clone URL (for example when SSH and HTTPS use different hosts/ports: `https://gitea.example.com:8443`).                                                                                                                                             |         |

### Required Token Permissions

The token used for commit status reporting needs permission to **write commit statuses** through the provider API. The same requirements apply whether you use the global [`GIT_ACCESS_TOKEN`](#authentication), a [domain-scoped token](#domain-scoped-authentication) from `GIT_AUTH_DOMAINS`, or a [GitHub App](#github-apps).

If an access token is also used to clone Git repositories over HTTP(S) (see [Authentication](#authentication)), add the permission below **in addition to** the existing clone permissions required by your provider (see [Required Token Permissions](#required-token-permissions) above).

!!! info "GitHub Apps reuse their installation token"
    When no `GIT_ACCESS_TOKEN` is configured (globally or per domain) but a GitHub App is, doco-cd reuses the same short-lived installation token used for git clone/fetch to post commit statuses. Grant the App the **Commit statuses: Read and write** repository permission; no additional PAT is required.

| Provider        | Token type                               | Required permission                                                                                                                                                                                                                 |
|-----------------|------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| GitHub          | Classic PAT / OAuth token                | `repo:status` is the minimum recommended scope. `repo` also works, but grants broader repository access than necessary.                                                                                                             |
| GitHub          | Fine-grained PAT / GitHub App            | Repository permission **Commit statuses: Read and write**                                                                                                                                                                           |
| GitLab          | Personal, project, or group access token | `api` scope                                                                                                                                                                                                                         |
| Gitea / Forgejo | API access token                         | Must be allowed to write repository API endpoints. On Forgejo scoped tokens, use `write:repository`. Some Gitea versions expose different token controls, so ensure the token can create commit statuses for the target repository. |
| Azure DevOps    | Personal Access Token (PAT)              | Token must allow Git status writes on the target repo (for example **Code (Read & write)** in the PAT scopes).                                                                                                                      |

!!! info "Why GitLab needs `api` instead of `write_repository`"
    Doco-CD posts commit statuses through the GitLab REST API. The `write_repository` scope covers Git-over-HTTP push access, but does not grant general REST API write access.

### Provider Auto-Detection

When `GIT_SCM_PROVIDER` is not set, doco-cd detects the provider from the repository URL:

| Hostname pattern                                           | Detected provider |
|------------------------------------------------------------|-------------------|
| `github.com`                                               | GitHub            |
| `gitlab.com`                                               | GitLab            |
| `dev.azure.com`, `ssh.dev.azure.com`, `*.visualstudio.com` | Azure DevOps      |
| Anything else                                              | Gitea / Forgejo   |

!!! warning "Self-hosted instances"
    Set `GIT_SCM_PROVIDER` explicitly when running a self-hosted SCM/Git provider instance and the auto-detection cannot determine the correct provider.

    - **GitHub Enterprise Server**: set `GIT_SCM_PROVIDER=github`
    - **Self-hosted GitLab**: set `GIT_SCM_PROVIDER=gitlab`
    - **Self-hosted Gitea / Forgejo**: set `GIT_SCM_PROVIDER=gitea` or `GIT_SCM_PROVIDER=forgejo`

### Self-Hosted Instances and API URL

Doco-CD derives the SCM API base URL from the clone URL by converting its scheme to `https://`. This works correctly for most setups, but two scenarios require an explicit override via `GIT_SCM_API_URL`:

**SSH clone URL with a non-standard SSH port**

When the clone URL is an SSH URL with a custom port (e.g. `ssh://git@gitea.example.com:2222/org/repo.git`), doco-cd strips the port before building the API URL so the API call targets `https://gitea.example.com/...` correctly. If for any reason this stripping doesn't produce the right host or port (e.g. the HTTPS endpoint is on a non-standard port), set the API base URL explicitly:

```yaml
GIT_SCM_API_URL: "https://gitea.example.com:8443"
```

**SSH and HTTPS served from different hostnames or ports**

Some self-hosted setups route SSH clones through a dedicated hostname or port (e.g. `git.internal:2222`) while the HTTPS API is on a different address (e.g. `https://git.example.com`). In that case the auto-derived URL will be wrong regardless of port stripping:

```yaml
GIT_SCM_API_URL: "https://git.example.com"
```

`GIT_SCM_API_URL` must be an `http://` or `https://` URL pointing to the root of the SCM instance (no path, no trailing slash). Provider-specific API paths (e.g. `/api/v1`, `/api/v4`) are appended automatically.

### Example

```yaml title="docker-compose.yml"
services:
  app:
    environment:
      GIT_ACCESS_TOKEN: xxx         # token must be allowed to write commit statuses
      GIT_COMMIT_STATUS: "true"
      # GIT_SCM_PROVIDER: gitlab   # uncomment for self-hosted GitLab at a custom domain
      # GIT_SCM_API_URL: https://git.example.com  # optional explicit API base URL override
```
