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

The following settings configure how Doco-CD authenticates with Git providers when cloning/pulling repositories.

You can use either 

- HTTP(S) authentication with access tokens
- SSH authentication with private keys.  
- For multiple domains/providers, see the [Domain-scoped authentication](#domain-scoped-authentication) section below.

| Key                               | Type   | Description                                                                                                                                                                                                                                                                             | Default                                          |
|-----------------------------------|--------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------|
| `AUTH_TYPE`                       | string | AuthType is the type of authentication to use when cloning repositories via **http**.                                                                                                                                                                                                   | `oauth2`                                         |
| `GIT_ACCESS_TOKEN`                | string | Access token for cloning repositories (required for private repositories) via **HTTP**, see [Access Token Setup](Setup-Access-Token.md). See also [Domain-scoped authentication](#domain-scoped-authentication).                                                                        | Optional for public repositories but recommended |
| `GIT_ACCESS_TOKEN_FILE`           | string | Path to the file containing the Git Access Token (mutually exclusive with `GIT_ACCESS_TOKEN`).                                                                                                                                                                                          |                                                  |
| `GIT_AUTH_DOMAINS`                | list   | YAML list of domain-scoped Git credentials (HTTP token, SSH key, and GitHub App credentials). Supports exact domains and wildcard subdomains like `*.example.com` (see [Domain-scoped authentication](#domain-scoped-authentication)). Mutually exclusive with `GIT_AUTH_DOMAINS_FILE`. |                                                  |
| `GIT_AUTH_DOMAINS_FILE`           | string | Path to a file containing the YAML configuration for `GIT_AUTH_DOMAINS` (mutually exclusive with `GIT_AUTH_DOMAINS`).                                                                                                                                                                   |                                                  |
| `SSH_PRIVATE_KEY`                 | string | The private key used for cloning repositories via SSH, see [SSH Key Setup](Setup-SSH-Key.md). See also [Domain-scoped authentication](#domain-scoped-authentication).                                                                                                                   |                                                  |
| `SSH_PRIVATE_KEY_FILE`            | string | Path to the file containing the SSH private key.                                                                                                                                                                                                                                        |                                                  |
| `SSH_PRIVATE_KEY_PASSPHRASE`      | string | Passphrase for the SSH private key (if the key was generated with a passphrase).                                                                                                                                                                                                        |                                                  |
| `SSH_PRIVATE_KEY_PASSPHRASE_FILE` | string | Path to the file containing the SSH private key passphrase.                                                                                                                                                                                                                             |                                                  |

## Domain-scoped Authentication

Use domain-scoped config when you fetch from multiple Git providers/domains and need separate credentials.

### Syntax and Format

The domain-scoped authentication configuration is a YAML list where each entry defines credentials for one or more domains.

#### Entry Structure

Each entry in the list has the following structure:

```yaml
- domains:                          # (Required) List of domain names or patterns
    - domain1.com
    - domain2.com
    - '*.example.com'
  git_access_token: xxx             # (Optional) HTTP token for git access
  ssh_private_key: |                # (Optional) SSH private key content
    -----BEGIN OPENSSH PRIVATE KEY-----
    ...
    -----END OPENSSH PRIVATE KEY-----
  ssh_private_key_passphrase: xxx   # (Optional) Passphrase for encrypted SSH key
```

#### Available Options

| Field                        | Type   | Required | Description                                                                                                          |
|------------------------------|--------|----------|----------------------------------------------------------------------------------------------------------------------|
| `domains`                    | list   | Yes      | List of domain names to apply these credentials to. Supports exact domains and wildcard patterns.                    |
| `git_access_token`           | string | No       | HTTP(S) access token for authenticating with the Git provider. Cannot be used with `ssh_private_key`.                |
| `ssh_private_key`            | string | No       | SSH private key content (multi-line). Cannot be used with `git_access_token`.                                        |
| `ssh_private_key_passphrase` | string | No       | Passphrase for the SSH private key if it was generated with encryption. Only used with `ssh_private_key`.            |
| `github_app_id`              | string | No       | GitHub App ID. Requires `github_app_private_key`. Cannot be used with `git_access_token` or `ssh_private_key`.       |
| `github_app_private_key`     | string | No       | GitHub App private key (PEM). Requires `github_app_id`. Cannot be used with `git_access_token` or `ssh_private_key`. |
| `github_app_installation_id` | number | No       | Optional installation ID override for this domain entry. If omitted, installation is auto-detected by owner/repo.    |

#### Authentication Method Selection

- **Use `git_access_token`** for HTTP(S) based Git access
- **Use `ssh_private_key`** (and optionally `ssh_private_key_passphrase`) for SSH-based Git access
- **Use `github_app_id` + `github_app_private_key`** for GitHub App based HTTP(S) access
- Do not mix methods in the same entry

### Matching Behavior

- Exact domain match wins over wildcard entries.
- If multiple wildcard patterns match, the longest suffix wins (most specific).
- Wildcards only match subdomains. Example: `*.example.com` matches `git.example.com`, but not `example.com`.
- If no domain entry matches, doco-cd falls back to global `GIT_ACCESS_TOKEN` / `SSH_PRIVATE_KEY` values if set.
- Submodule remotes are resolved independently, so each submodule can use credentials for its own domain.

### Examples

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

## GitHub Apps

[GitHub Apps](https://docs.github.com/en/apps) are supported natively and can be configured globally (see below) or [per domain](#domain-scoped-authentication). 
Doco-CD will auto-detect the installation by repository _owner/name_ and mint short-lived installation access tokens.

| Key                             | Type   | Description                                                                                                                                                                  | Default value     |
|---------------------------------|--------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------|
| `GITHUB_APP_ID`                 | string | ID of the GitHub App, used to mint installation access tokens for GitHub repositories. Requires `GITHUB_APP_PRIVATE_KEY`. Mutually exclusive with global `GIT_ACCESS_TOKEN`. |                   |
| `GITHUB_APP_ID_FILE`            | string | Path to the file containing `GITHUB_APP_ID` (mutually exclusive with `GITHUB_APP_ID`).                                                                                       |                   |
| `GITHUB_APP_PRIVATE_KEY`        | string | GitHub App private key in PEM format. Requires `GITHUB_APP_ID`.                                                                                                              |                   |
| `GITHUB_APP_PRIVATE_KEY_FILE`   | string | Path to the file containing `GITHUB_APP_PRIVATE_KEY` (mutually exclusive with `GITHUB_APP_PRIVATE_KEY`).                                                                     |                   |
| `GITHUB_APP_INSTALLATION_ID`    | number | Optional installation ID override for global GitHub App auth. If unset, doco-cd resolves installation by _owner/repository_ automatically.                                   | `0` (auto-detect) |
