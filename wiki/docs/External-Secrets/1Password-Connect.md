---
tags:
  - Advanced
  - Secrets
  - Configuration
---

# 1Password Connect Server

[1Password Connect Server](https://developer.1password.com/docs/connect/) is a self-hosted proxy that caches vault data locally and serves secrets over a simple HTTP API. This is useful when you are deploying frequently or have multiple instances that would otherwise hit 1Password API rate limits.

Unlike service account authentication (which makes direct calls to the 1Password cloud API), Connect Server allows you to:

- Avoid rate limiting by caching vault data locally
- Reduce latency for secret lookups
- Keep all secret requests within your infrastructure

!!! info
    Client-side caching is automatically disabled when using Connect Server because Connect already handles caching for you.

## Environment Variables

To use 1Password Connect, configure these variables for the `doco-cd` container:

| Key                                  | Value                                                                                                  |
|--------------------------------------|--------------------------------------------------------------------------------------------------------|
| `SECRET_PROVIDER`                    | `1password`                                                                                            |
| `SECRET_PROVIDER_CONNECT_HOST`       | Base URL of your Connect API Server (for example: `http://op-connect-api:8080`).                       |
| `SECRET_PROVIDER_CONNECT_TOKEN`      | API token used by `doco-cd` to authenticate against Connect API. Generated in 1Password Connect setup. |
| `SECRET_PROVIDER_CONNECT_TOKEN_FILE` | Path to the file containing the Connect API token inside the container.                                |

!!! info
    Use **either** `SECRET_PROVIDER_CONNECT_TOKEN` **or** `SECRET_PROVIDER_CONNECT_TOKEN_FILE`.

For the Connect containers themselves, you also need a credentials file:

| File | Purpose | Source |
|------|---------|--------|
| `./1password-credentials.json` | Authenticates `op-connect-api`/`op-connect-sync` with your 1Password account and allows vault sync. | Downloaded from your 1Password Connect setup |

## Setup Steps

### Example Compose Setup

Deploy 1Password Connect alongside doco-cd:

```yaml title="docker-compose.yml" hl_lines="2-16 21-25 27-28"
services:
  op-connect-api:
    image: 1password/connect-api:latest
    ports:
      - "8080:8080"
    volumes:
      - "./1password-credentials.json:/home/opuser/.op/1password-credentials.json"
      - "op_data:/home/opuser/.op/data"

  op-connect-sync:
    image: 1password/connect-sync:latest
    ports:
      - "8081:8080"
    volumes:
      - "./1password-credentials.json:/home/opuser/.op/1password-credentials.json"
      - "op_data:/home/opuser/.op/data"

  doco-cd:
    image: kimdre/doco-cd:latest
    environment:
      SECRET_PROVIDER: 1password
      SECRET_PROVIDER_CONNECT_HOST: http://op-connect-api:8080
      SECRET_PROVIDER_CONNECT_TOKEN: ${SECRET_PROVIDER_CONNECT_TOKEN}
    depends_on:
      - op-connect-api

volumes:
  op_data:
```

Example `.env` values for the compose file above:

```bash
# Used by doco-cd to call op-connect-api
SECRET_PROVIDER_CONNECT_TOKEN=xxxxxx
```

Place `1password-credentials.json` next to your compose file (as shown above), or adjust the bind mount path to your preferred secure location.


### Configuring doco-cd to use Connect Server

Set these [environment variables](#environment-variables) to use 1Password Connect instead of service account authentication:

```bash
SECRET_PROVIDER=1password
SECRET_PROVIDER_CONNECT_HOST=http://op-connect-api:8080
SECRET_PROVIDER_CONNECT_TOKEN=your-connect-token
```

Or use a file-based token:

```bash
SECRET_PROVIDER=1password
SECRET_PROVIDER_CONNECT_HOST=http://op-connect-api:8080
SECRET_PROVIDER_CONNECT_TOKEN_FILE=/run/secrets/op_connect_token
```

!!! tip
    Prefer `SECRET_PROVIDER_CONNECT_TOKEN_FILE` in production so the token is mounted as a secret file instead of a plain environment variable.

### Getting your Connect Server credentials

For detailed setup instructions, refer to the [1Password Connect Server documentation](https://developer.1password.com/docs/connect/).

To generate a Connect token:

1. Open 1Password
2. Go to the Admin Console
3. Select the 1Password Connect integration (or create a new one)
4. Generate a new token
5. Save the token in a secure location

To generate the `1password-credentials.json` file used by `op-connect-api` and `op-connect-sync`, follow the [1Password Connect authentication guide](https://developer.1password.com/docs/connect/authentication/).

