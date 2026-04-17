---
tags:
  - External Secrets
  - Configuration
---

# External Secrets

External secrets are secrets that are stored in an external secret management service and fetched during a deployment by Doco-CD.
This allows you to keep your secrets out of your Git repository and manage them in a secure way.

## Supported Secret Provider

| Provider                                                        | More Information                                                                                |
|-----------------------------------------------------------------|-------------------------------------------------------------------------------------------------|
| [AWS Secrets Manager](aws-secrets-manager.md)                   | https://docs.aws.amazon.com/secretsmanager/latest/userguide/intro.html                          |
| [Bitwarden Secrets Manager](bitwarden-secrets-manager.md)       | https://bitwarden.com/products/secrets-manager/                                                 |
| [Bitwarden Vault / Vaultwarden](bitwarden-vault-vaultwarden.md) | https://bitwarden.com/help/vault-management-api/ and https://github.com/dani-garcia/vaultwarden |
| [1Password](1password.md)                                       | https://1password.com                                                                           |
| [Infisical](infisical.md)                                       | https://infisical.com/                                                                          |
| [OpenBao](openbao.md)                                           | https://openbao.org/                                                                            |
| [Webhook](webhook.md)                                           | Fetch secrets from any remote service via HTTP requests with a flexible configuration           |

!!! tip
    Additional external secret providers may be supported in the future. If you have a specific provider in mind, please [open a feature request](https://github.com/kimdre/doco-cd/issues/new?template=feature-request.yml) or [submit a pull request](https://github.com/kimdre/doco-cd/compare) if you are able to implement the provider yourself.

## Setting up an External Secret Provider

To use an external secret provider, configure the environment variables for your provider and then set `external_secrets` in your `.doco-cd.yml`.
See the provider-specific pages for details.

## Using External Secrets in Deployments

Doco-CD uses variable interpolation to replace variables in your Compose files with the values fetched from the external secret provider, see the [Compose file reference](https://docs.docker.com/reference/compose-file/interpolation/) for more information and examples.

For example with [Bitwarden Secrets Manager](bitwarden-secrets-manager.md), if you want to use secrets named `DB_PASSWORD` and `LABEL_SECRET` in your Compose file, you can reference it like this:

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
