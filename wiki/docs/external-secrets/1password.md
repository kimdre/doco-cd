# 1Password

!!! info
    The start time and memory usage of the doco-cd container, as well as the runtime of a job, can increase significantly when using this secret provider.

## Environment Variables

To use 1Password, you need to set the following environment variables:

| Key                                 | Value                                                                                                                                                                                                                              |
|-------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `SECRET_PROVIDER`                   | `1password`                                                                                                                                                                                                                        |
| `SECRET_PROVIDER_ACCESS_TOKEN`      | Access token of a service account, see [the docs](https://developer.1password.com/docs/service-accounts/security/) and [here](https://developer.1password.com/docs/sdks/setup-tutorial/#part-1-set-up-a-1password-service-account) |
| `SECRET_PROVIDER_ACCESS_TOKEN_FILE` | Path to the file containing the access token inside the container                                                                                                                                                                  |

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
