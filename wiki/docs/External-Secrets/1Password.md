---
tags:
  - Advanced
  - Secrets
  - Configuration
---

# 1Password

1Password runs as a [gRPC plugin container](index.md#plugin-architecture) (`ghcr.io/kimdre/doco-cd-secretprovider-1password`) sitting next to `doco-cd`.

!!! info
    The start time and memory usage of the plugin container, as well as the runtime of a job, can increase significantly when using this secret provider.

!!! tip "Using 1Password Connect Server"
    For improved performance and to avoid API rate limits in high-volume deployments, consider using [1Password Connect Server](1Password-Connect.md) instead of service account authentication.

## Environment Variables

### `doco-cd` container

| Key                             | Value                                                                            |
|---------------------------------|----------------------------------------------------------------------------------|
| `SECRET_PROVIDER`               | `grpc`                                                                           |
| `SECRET_PROVIDER_GRPC_ENDPOINT` | Endpoint of the plugin. Default: `unix:///var/run/doco-cd/secret-provider.sock`. |

### Plugin container (`doco-cd-secretprovider-1password`)

| Key                                  | Value                                                                                                                                                                                                                              |
|--------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `SECRET_PROVIDER_GRPC_ENDPOINT`      | Endpoint the plugin listens on (must match the value on `doco-cd`).                                                                                                                                                                |
| `SECRET_PROVIDER_ACCESS_TOKEN`       | Access token of a service account, see [the docs](https://developer.1password.com/docs/service-accounts/security/) and [here](https://developer.1password.com/docs/sdks/setup-tutorial/#part-1-set-up-a-1password-service-account) |
| `SECRET_PROVIDER_ACCESS_TOKEN_FILE`  | Path to the file containing the service account token inside the container                                                                                                                                                         |

### Example compose layout

```yaml title="docker-compose.yml"
services:
  doco-cd:
    image: ghcr.io/kimdre/doco-cd:latest
    environment:
      SECRET_PROVIDER: grpc
    volumes:
      - secret-provider-sock:/var/run/doco-cd
    depends_on:
      - secret-provider

  secret-provider:
    image: ghcr.io/kimdre/doco-cd-secretprovider-1password:latest
    environment:
      SECRET_PROVIDER_ACCESS_TOKEN: ${OP_SERVICE_ACCOUNT_TOKEN}
    volumes:
      - secret-provider-sock:/var/run/doco-cd

volumes:
  secret-provider-sock:
```

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

Optional client-side caching[^1] reduces 1Password API calls when using service account authentication. Set these on the plugin container:

| Key                              | Type      | Value                                                                                                                            | Default |
|----------------------------------|-----------|----------------------------------------------------------------------------------------------------------------------------------|:--------|
| `SECRET_PROVIDER_CACHE_ENABLED`  | `boolean` | Enables in-memory caching for resolved secrets                                                                                   | `false` |
| `SECRET_PROVIDER_CACHE_TTL`      | `string`  | Cache TTL for resolved secrets as a [Go duration](https://pkg.go.dev/time#ParseDuration) string (for example: `30s`, `5m`, `1h`) | `5m`    |
| `SECRET_PROVIDER_CACHE_MAX_SIZE` | `number`  | Maximum number of secrets stored in cache before least-recently-used entries are evicted                                         | `100`   |

!!! warning "If the cache TTL is too long, secrets may become outdated."

[^1]: 
    Client-side caching can only be used with service account authentication. 
    When using [1Password Connect Server](1Password-Connect.md), client-side caching is automatically disabled because the Connect Server already handles caching for you.
