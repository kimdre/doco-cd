---
tags:
  - Advanced
  - Secrets
  - Configuration
---

# 1Password

!!! info
    The start time and memory usage of the doco-cd container, as well as the runtime of a job, can increase significantly when using this secret provider.

!!! tip "Using 1Password Connect Server"
    For improved performance and to avoid API rate limits in high-volume deployments, consider using [1Password Connect Server](1Password-Connect.md) instead of service account authentication.

## Environment Variables

To use 1Password, configure these variables for the `doco-cd` container

| Key                                  | Value                                                                                                                                                                                                                              |
|--------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `SECRET_PROVIDER`                    | `1password`                                                                                                                                                                                                                        |
| `SECRET_PROVIDER_ACCESS_TOKEN`       | Access token of a service account, see [the docs](https://developer.1password.com/docs/service-accounts/security/) and [here](https://developer.1password.com/docs/sdks/setup-tutorial/#part-1-set-up-a-1password-service-account) |
| `SECRET_PROVIDER_ACCESS_TOKEN_FILE`  | Path to the file containing the service account token inside the container                                                                                                                                                         |

!!! tip "API Rate Limit"
    If you hit the API rate limit, you can also enable client-side caching for resolved secrets. See the [Client-Side Caching](#client-side-caching) section below for more details.

## Deployment configuration

Add a mapping/reference between the environment variable you want to set in the docker compose project/stack and the URI to the secret in 1Password.

See their docs for the correct syntax and how to get a secret reference of your secret: https://developer.1password.com/docs/cli/secret-reference-syntax/

A valid secret reference should use the syntax:
`op://<vault>/<item>/[section/]<field>`

To get a one-time password, append the `?attribute=otp` query parameter to a secret reference that points to a one-time password field in 1Password:
`op://<vault>/<item>/[section/]one-time password?attribute=otp`

!!! warning
    Machine accounts can only access vaults for which you have granted read permissions during creation. The default `Personal` vault can't be access by machine accounts!

### Example

For example in your `.doco-cd.yml`:

```yaml title=".doco-cd.yml"
name: myapp
external_secrets:
  DB_PASSWORD: "op://vault/item/field"
```

## Client-Side Caching

Optional client-side caching reduces 1Password API calls when using service account authentication. Enable and configure caching with the following environment variables:

| Key                              | Type                                                | Value                                                                                    | Default |
|----------------------------------|-----------------------------------------------------|------------------------------------------------------------------------------------------|:--------|
| `SECRET_PROVIDER_CACHE_ENABLED`  | `boolean`                                           | Enables in-memory caching for resolved secrets                                           | `false` |
| `SECRET_PROVIDER_CACHE_TTL`      | [`duration`](https://pkg.go.dev/time#ParseDuration) | Cache TTL for resolved secrets as a Go duration string (for example: `30s`, `5m`, `1h`)  | `5m`    |
| `SECRET_PROVIDER_CACHE_MAX_SIZE` | `int`                                               | Maximum number of secrets stored in cache before least-recently-used entries are evicted | `100`   |

!!! warning "If the cache TTL is too long, secrets may become outdated."

!!! info
    Client-side caching can only be used with service account authentication. When using [1Password Connect Server](1Password-Connect.md), client-side caching is automatically disabled because Connect already handles caching for you.
