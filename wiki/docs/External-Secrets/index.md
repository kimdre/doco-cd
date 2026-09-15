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
| [Azure Key Vault](Azure-Key-Vault.md)                           | https://azure.microsoft.com/en-us/products/key-vault                                            |
| [Bitwarden Secrets Manager](Bitwarden-Secrets-Manager.md)       | https://bitwarden.com/products/secrets-manager/                                                 |
| [Bitwarden Vault / Vaultwarden](Bitwarden-Vault-Vaultwarden.md) | https://bitwarden.com/help/vault-management-api/ and https://github.com/dani-garcia/vaultwarden |
| [1Password](1Password.md)                                       | https://1password.com                                                                           |
| [Infisical](Infisical.md)                                       | https://infisical.com/                                                                          |
| [OpenBao](Openbao.md)                                           | https://openbao.org/                                                                            |
| [Webhook](Webhook.md)                                           | Fetch secrets from any remote service via HTTP requests with a flexible configuration           |

!!! tip
    Additional external secret providers may be supported in the future. If you have a specific provider in mind, please [open a feature request](https://github.com/kimdre/doco-cd/issues/new?template=feature-request.yml) or [submit a pull request](https://github.com/kimdre/doco-cd/compare) if you are able to implement the provider yourself.

## Setting up an External Secret Provider

To use an external secret provider, configure the environment variables for your provider and then set `external_secrets` in your `.doco-cd.yml`.
See the provider-specific pages for details.

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

### With Interpolation

External-secret references can optionally support Compose-style interpolation using environment variables set on the doco-cd deployment itself. 
Set `INTERPOLATE_EXTERNAL_SECRETS=true` to enable this feature. It is disabled by default:

```yaml title=".doco-cd.yml"
name: myapp
external_secrets:
  DB_PASSWORD: "kv:db-${PROJECT_STAGE:-prod}"
```

With `INTERPOLATE_EXTERNAL_SECRETS=true` and `PROJECT_STAGE=lab` set in the doco-cd container, the provider receives `kv:db-lab`. 
If `PROJECT_STAGE` is not set, the default `prod` is used. 

!!! note "Only legacy string references are interpolated"
    Structured webhook references are preserved unchanged. 

!!! info "Only use with trusted external secret references"
    Enable this option only when the external secret references are trusted, because referenced process environment variables are included in provider requests.

### Defining External Secrets in a File

Instead of (or in addition to) declaring `external_secrets` inline in your `.doco-cd.yml`, you can set `external_secrets_files` to a list of YAML files, each holding a map of env var name to external secret reference using the same shape as `external_secrets`:

```yaml title=".doco-cd.yml"
name: myapp
external_secrets_files:
  - secrets.yaml
```

```yaml title="secrets.yaml"
DB_PASSWORD: a8f1e4eb-d76d-47b4-aa3c-103733e77fce
LABEL_SECRET: cfd0c4a9-16d4-44c8-9a80-c6143a7c7b71
```

File paths are resolved relative to the same directory as [dotenv files](../Deploy-Settings.md#dotenv-file-format). Like `env_files`, you can use the `remote:<filepath>` syntax to load a file from the remote repository when `repository_url` is also specified:

```yaml title=".doco-cd.yml"
name: myapp
repository_url: https://github.com/example/other-repo.git
external_secrets_files:
  - remote:secrets.yaml
```

Files can also be [SOPS-encrypted](../Advanced/Encryption.md), the same way encrypted dotenv files are supported.

!!! note "Inline entries take precedence"
    `external_secrets_files` are merged first, then `external_secrets` entries are applied on top. If the same name is defined in both, the inline value from `external_secrets` wins.

### Referencing Other Resolved Secrets

A resolved secret's value can itself reference another external secret by name, letting you compose one secret from others without duplicating values in your Compose file.
Set `INTERPOLATE_RESOLVED_SECRETS=true` to enable this feature. It is disabled by default:

```yaml title=".doco-cd.yml"
name: myapp
external_secrets:
  DB_PASSWORD: <secret-ref-a>
  DB_HOST: <secret-ref-b>
  DB_URL: <secret-ref-c> # resolves to: postgres://user:${DB_PASSWORD}@${DB_HOST}/mydb
```

With `INTERPOLATE_RESOLVED_SECRETS=true`, if the secret provider returns `postgres://user:${DB_PASSWORD}@${DB_HOST}/mydb` for `DB_URL`, doco-cd substitutes `${DB_PASSWORD}` and `${DB_HOST}` with the values of the other resolved external secrets before the value is made available to the Compose file. References can be chained across multiple secrets.

!!! note "Only other external secrets are used"
    Unlike [reference interpolation](#with-interpolation), this only substitutes names that match another entry in `external_secrets`. The doco-cd process environment is never consulted, so an unrelated `${VAR}` left in a secret's value (e.g. matching an OS environment variable) is left untouched.

!!! warning "Circular references fail the deployment"
    If secret `A`'s value references secret `B`, and `B`'s value references `A`, doco-cd returns an error instead of interpolating.
