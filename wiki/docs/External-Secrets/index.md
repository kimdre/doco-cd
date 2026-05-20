---
tags:
  - Advanced
  - Secrets
  - Configuration
---

# External Secrets

External secrets are secrets that are stored in an external secret management service and fetched during a deployment by Doco-CD.
This allows you to keep your secrets out of your Git repository and manage them in a secure way.

## Supported Secret Provider

| Provider                                                        | More Information                                                                                |
|-----------------------------------------------------------------|-------------------------------------------------------------------------------------------------|
| [AWS Secrets Manager](AWS-Secrets-Manager.md)                   | https://docs.aws.amazon.com/secretsmanager/latest/userguide/intro.html                          |
| [Bitwarden Secrets Manager](Bitwarden-Secrets-Manager.md)       | https://bitwarden.com/products/secrets-manager/                                                 |
| [Bitwarden Vault / Vaultwarden](Bitwarden-Vault-Vaultwarden.md) | https://bitwarden.com/help/vault-management-api/ and https://github.com/dani-garcia/vaultwarden |
| [1Password](1Password.md)                                       | https://1password.com                                                                           |
| [Infisical](Infisical.md)                                       | https://infisical.com/                                                                          |
| [OpenBao](Openbao.md)                                           | https://openbao.org/                                                                            |
| [Webhook](Webhook.md)                                           | Fetch secrets from any remote service via HTTP requests with a flexible configuration           |

!!! tip
    Additional external secret providers may be supported in the future. If you have a specific provider in mind, please [open a feature request](https://github.com/kimdre/doco-cd/issues/new?template=feature-request.yml) or [submit a pull request](https://github.com/kimdre/doco-cd/compare) if you are able to implement the provider yourself.

## Plugin Architecture

External secret providers (except [Webhook](Webhook.md)) run as **standalone gRPC plugin containers** sitting next to the `doco-cd` container.
Each provider has its own image (`ghcr.io/kimdre/doco-cd-secretprovider-<name>`) and listens on a Unix socket that `doco-cd` connects to.

This means:

- The `doco-cd` core no longer bundles upstream SDKs (smaller image, no CGO).
- Plugins can be upgraded, restricted, or removed independently.
- Each plugin only needs its own provider credentials; `doco-cd` only needs to know where to reach it.

The connection happens over a shared Unix socket mounted into both containers (`/var/run/doco-cd/secret-provider.sock` by default).

### Common configuration

| Key                                | Used in container | Value                                                                                                                |
|------------------------------------|-------------------|----------------------------------------------------------------------------------------------------------------------|
| `SECRET_PROVIDER`                  | `doco-cd`         | `grpc` (use any external plugin) or `webhook` (built-in HTTP provider, see [Webhook](Webhook.md)).                    |
| `SECRET_PROVIDER_GRPC_ENDPOINT`    | `doco-cd`, plugin | Endpoint URL the plugin listens on / the core dials. Default: `unix:///var/run/doco-cd/secret-provider.sock`. `tcp://host:port` also supported. |
| `SECRET_PROVIDER_GRPC_DIAL_TIMEOUT`| `doco-cd`         | Connection timeout when reaching the plugin. Default: `10s`.                                                         |

`doco-cd` is plugin-agnostic: it only knows how to dial the gRPC endpoint. Which backend serves the requests is determined entirely by the image you run as the plugin container.

### Example compose layout

```yaml title="docker-compose.yml"
services:
  doco-cd:
    image: ghcr.io/kimdre/doco-cd:latest
    environment:
      SECRET_PROVIDER: grpc
      SECRET_PROVIDER_GRPC_ENDPOINT: unix:///var/run/doco-cd/secret-provider.sock
    volumes:
      - secret-provider-sock:/var/run/doco-cd
    depends_on:
      - secret-provider

  secret-provider:
    image: ghcr.io/kimdre/doco-cd-secretprovider-openbao:latest
    environment:
      SECRET_PROVIDER_GRPC_ENDPOINT: unix:///var/run/doco-cd/secret-provider.sock
      SECRET_PROVIDER_SITE_URL: https://bao.example.com
      SECRET_PROVIDER_ACCESS_TOKEN: ${SECRET_PROVIDER_ACCESS_TOKEN}
    volumes:
      - secret-provider-sock:/var/run/doco-cd

volumes:
  secret-provider-sock:
```

## Setting up an External Secret Provider

Run the matching plugin image next to `doco-cd`, share the socket volume, and point `doco-cd` at the same endpoint. Then set `external_secrets` in your `.doco-cd.yml`.
See the provider-specific pages for the env vars each plugin accepts.

## Using External Secrets in Deployments

Doco-CD uses variable interpolation to replace variables in your Compose files with the values fetched from the external secret provider, see the [Compose file reference](https://docs.docker.com/reference/compose-file/interpolation/) for more information and examples.

For example with [Bitwarden Secrets Manager](Bitwarden-Secrets-Manager.md), if you want to use secrets named `DB_PASSWORD` and `LABEL_SECRET` in your Compose file, you can reference it like this:

```yaml title=".doco-cd.yml"
name: myapp
external_secrets:
  DB_PASSWORD: a8f1e4eb-d76d-47b4-aa3c-103733e77fce
  LABEL_SECRET: cfd0c4a9-16d4-44c8-9a80-c6143a7c7b71
```

Then you can use the variable in your Compose file like this:

!!! warning
    External secrets have a higher priority than variables set in a `.env` file or in the environment.
    If a variable is set in both an external secret and in a `.env` file, the value from the external secret will be used.


```dotenv title=".env"
DB_PASSWORD=testpassword # This will be overridden by the external secret
DOMAIN=example.com
```

```yaml title="docker-compose.yml" hl_lines="6-7 10 14-16"
services:
  app:
    image: myapp:latest
    environment:
      DATABASE_HOST: db
      DATABASE_USER: ${$DB_USER:-postgres} # You can also set a default value if the secret is missing
      DATABASE_PASSWORD: $DB_PASSWORD
    labels:
      - "traefik.enable=true"
      - "traefik.http.routers.myapp.rule=Host(`myapp.${DOMAIN}`)"  # Note that DOMAIN is set in a local .env file and not fetched from the secret provider
  db:
    image: postgres:latest
    environment:
      POSTGRES_USER: ${$DB_USER:-postgres}
      POSTGRES_PASSWORD: ${DB_PASSWORD}
```

This will result in the following docker-compose configuration being used during deployment:

```yaml title="docker-compose.yml" hl_lines="6-7 10 14-16"
services:
  app:
    image: myapp:latest
    environment:
      DATABASE_HOST: db
      DATABASE_USER: postgres
      DATABASE_PASSWORD: supersecretpassword123 # Value of external secret fetched from Bitwarden Secrets Manager
    labels:
      - "traefik.enable=true"
      - "traefik.http.routers.myapp.rule=Host(`myapp.example.com`)"  # Value from .env file
  db:
    image: postgres:latest
    environment:
      POSTGRES_USER: postgres
      POSTGRES_PASSWORD: supersecretpassword123 # Value of external secret fetched from Bitwarden Secrets Manager
```
