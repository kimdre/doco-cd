---
tags:
  - Advanced
  - Secrets
  - Configuration
---

# Infisical

## Environment Variables

To use Infisical, you need to set the following environment variables:

| Key                                  | Value                                                                                                                                                    |
|--------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------|
| `SECRET_PROVIDER`                    | `infisical`                                                                                                                                              |
| `SECRET_PROVIDER_SITE_URL`           | The URL of the Infisical site (e.g. `https://app.infisical.com`, `https://eu.infisical.com` or your self-hosted instance URL)                            |
| `SECRET_PROVIDER_CLIENT_ID`          | The Client ID of a machine account, see the docs for [machine accounts](https://infisical.com/docs/documentation/platform/identities/machine-identities) |
| `SECRET_PROVIDER_CLIENT_SECRET`      | The Client Secret of a machine account ([Universal Auth](https://infisical.com/docs/documentation/platform/identities/universal-auth))                   |
| `SECRET_PROVIDER_CLIENT_SECRET_FILE` | Path to the file containing the client secret inside the container                                                                                       |

## Deployment configuration

Add a mapping/reference between the environment variable you want to set in the docker compose project/stack and the reference to the secret in Infisical.

A valid secret reference should use the syntax:
`projectId:env:[/some/path/]key`

!!! warning
    Machine accounts can only access projects for which you have granted read permissions.
    When using [Infisical secret references](https://infisical.com/docs/documentation/platform/secret-reference),
    the machine identity also needs read access to every referenced secret and its environment and folder.

### Example

For example in your `.doco-cd.yml`:

```yaml title=".doco-cd.yml"
name: myapp
external_secrets:
  TEST_PASSWORD: 0db45926-c97c-40d4-a3aa-fefd5d5fb492:dev:DATABASE_URL
  OTHER_PASSWORD: "0db45926-c97c-40d4-a3aa-fefd5d5fb492:dev:/Test/Sub/TEST_SECRET"
  USERNAME: 0db45926-c97c-40d4-a3aa-fefd5d5fb492:dev:Test/Sub/TEST_SECRET
```

## Infisical secret references

You can reference other Infisical secrets, including imported secrets, in a
secret's value. Define expressions such as `${DB_HOST}` in the value stored in
Infisical, not in the `external_secrets` locator.

For example, an Infisical secret named `DATABASE_URL` can contain:

```text
postgres://${DB_HOST}/myapp
```

The `.doco-cd.yml` file continues to identify that secret using the doco-cd
locator format:

```yaml title=".doco-cd.yml"
external_secrets:
  DATABASE_URL: 0db45926-c97c-40d4-a3aa-fefd5d5fb492:prod:DATABASE_URL
```

Infisical resolves `${DB_HOST}` before doco-cd injects `DATABASE_URL` into the
Compose project.

!!! warning "Validate reference permissions"
    Infisical secret-reference expansion requires access to every secret in the
    reference chain. Depending on the Infisical server and API behavior, a
    resolution problem can fail the request or leave reference text unresolved.
    The SDK does not expose a separate "fully resolved" status, so doco-cd trusts
    and injects the value returned by Infisical. Validate the machine identity's
    permissions and the expanded value in Infisical before deployment.

## Combining both interpolation layers

`INTERPOLATE_EXTERNAL_SECRETS` applies only to the locator in `.doco-cd.yml`.
Infisical secret references apply later, inside the value fetched from
Infisical. For example:

```yaml title=".doco-cd.yml"
external_secrets:
  DATABASE_URL: "0db45926-c97c-40d4-a3aa-fefd5d5fb492:${PROJECT_STAGE:-prod}:DATABASE_URL"
```

With `INTERPOLATE_EXTERNAL_SECRETS=true`, doco-cd first uses `PROJECT_STAGE` to
select the Infisical environment. Infisical then retrieves `DATABASE_URL` from
that environment and expands references such as `${DB_HOST}` in its stored
value.
