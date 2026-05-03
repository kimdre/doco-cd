---
tags:
  - Advanced
  - Secrets
  - Configuration
---

# 1Password Connect Server

[1Password Connect Server](https://developer.1password.com/docs/connect/) is a self-hosted proxy that caches vault data locally and serves secrets over a simple HTTP API. This is useful when you are deploying frequently or have multiple instances that would otherwise hit 1Password API rate limits.

Unlike service account authentication (see the [1Password provider](1Password.md)) (which makes direct calls to the 1Password cloud API), Connect Server allows you to:

- Avoid rate limiting by caching vault data locally
- Reduce latency for secret lookups
- Keep all secret requests within your infrastructure

## Environment Variables

To use 1Password Connect, configure these variables for the `doco-cd` container:

| Key                                  | Value                                                                                                                                                                                           |
|--------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `SECRET_PROVIDER`                    | `1password`                                                                                                                                                                                     |
| `SECRET_PROVIDER_CONNECT_HOST`       | Base URL of your Connect API Server (for example: `http://op-connect-api:8080`).                                                                                                                |
| `SECRET_PROVIDER_CONNECT_TOKEN`      | API token used by `doco-cd` to authenticate against Connect API. Generated in [1Password Connect setup][1password-connect-setup]. Mutually exclusive with `SECRET_PROVIDER_CONNECT_TOKEN_FILE`. |
| `SECRET_PROVIDER_CONNECT_TOKEN_FILE` | Path to the file containing the Connect API token inside the container. Mutually exclusive with `SECRET_PROVIDER_CONNECT_TOKEN`.                                                                |

For the Connect containers themselves, you also need a `1password-credentials.json` credentials file 
to authenticate `op-connect-api`/`op-connect-sync` with your 1Password account and allow vault sync.
Download it from your [1Password Connect setup][1password-connect-setup].

## Setup Steps

### Example Compose Setup

Deploy 1Password Connect alongside doco-cd:

- Follow the [1Password Connect Server documentation](https://developer.1password.com/docs/connect/get-started/?deploy=docker) to get your Connect server credentials and set up the `op-connect-api` and `op-connect-sync` containers.
- For the server configuration options, refer to the [1Password Connect Server Configuration](https://developer.1password.com/docs/connect/server-configuration/) docs.
- Place `1password-credentials.json` next to your compose file (as shown below), or adjust the bind mount path to your preferred secure location (For a token file example, see the [Using a token file](#configuring-doco-cd-to-authenticate-with-connect-server-using-a-token-file) section below).

```yaml title="docker-compose.yml" hl_lines="2-16 21-25 27-28"
services:
  op-connect-api:
    image: 1password/connect-api:latest
    ports:
      - "8080:8080"
    volumes:
      - ./1password-credentials.json:/home/opuser/.op/1password-credentials.json # (1)!
      - op_data:/home/opuser/.op/data

  op-connect-sync:
    image: 1password/connect-sync:latest
    ports:
      - "8081:8080"
    volumes:
      - ./1password-credentials.json:/home/opuser/.op/1password-credentials.json # (2)!
      - op_data:/home/opuser/.op/data

  app: # your doco-cd container
    image: kimdre/doco-cd:latest
    environment:
      SECRET_PROVIDER: 1password
      SECRET_PROVIDER_CONNECT_HOST: http://op-connect-api:8080
      SECRET_PROVIDER_CONNECT_TOKEN: ${SECRET_PROVIDER_CONNECT_TOKEN} # (3)!
    depends_on:
      - op-connect-api

volumes:
  op_data:
```

1. Download the `1password-credentials.json` file from your [Secrets Automation workflow][1password-connect-setup] and mount it into both `op-connect-api` and `op-connect-sync` containers.
2. Download the `1password-credentials.json` file from your [Secrets Automation workflow][1password-connect-setup] and mount it into both `op-connect-api` and `op-connect-sync` containers.
3. Create the Connect server _Secrets Automation_ workflow by following the [docs][1password-connect-setup]. 

Example `.env` values for the compose file above:

```bash title=".env"
SECRET_PROVIDER_CONNECT_TOKEN=xxxxxx # (1)!
```

1. Used by doco-cd to call op-connect-api

### Configuring doco-cd to authenticate with Connect Server

Set these [environment variables](#environment-variables) to use 1Password Connect Sever in your `doco-cd` container:

=== "Using a direct token"

    ```bash title="Environment Variables for doco-cd"
    SECRET_PROVIDER=1password
    SECRET_PROVIDER_CONNECT_HOST=http://op-connect-api:8080
    SECRET_PROVIDER_CONNECT_TOKEN=your-connect-token
    ```

=== "Using a token file"

    ```bash title="Environment Variables for doco-cd"
    SECRET_PROVIDER=1password
    SECRET_PROVIDER_CONNECT_HOST=http://op-connect-api:8080
    SECRET_PROVIDER_CONNECT_TOKEN_FILE=/run/secrets/op_connect_token
    ```

    Mount the Connect token file as a secret or volume into the `doco-cd` container at the specified path:

    ```yaml title="docker-compose.yml"
    services:
      app:
        image: kimdre/doco-cd:latest
        environment:
          SECRET_PROVIDER: 1password
          SECRET_PROVIDER_CONNECT_HOST: http://op-connect-api:8080
          SECRET_PROVIDER_CONNECT_TOKEN_FILE: /run/secrets/op_connect_token
        secrets:
          - op_connect_token
    
    secrets:
      op_connect_token:
        file: ./op_connect_token.txt
    ```

[1password-connect-setup]: https://developer.1password.com/docs/connect/get-started/?deploy=docker#step-1