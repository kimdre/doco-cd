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

### Example

For example in your `.doco-cd.yml`:

```yaml title=".doco-cd.yml"
name: myapp
external_secrets:
  TEST_PASSWORD: 0db45926-c97c-40d4-a3aa-fefd5d5fb492:dev:DATABASE_URL
  OTHER_PASSWORD: "0db45926-c97c-40d4-a3aa-fefd5d5fb492:dev:/Test/Sub/TEST_SECRET"
  USERNAME: 0db45926-c97c-40d4-a3aa-fefd5d5fb492:dev:Test/Sub/TEST_SECRET
```
