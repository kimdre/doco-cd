---
tags:
  - Advanced
  - Secrets
  - Configuration
---

# Azure Key Vault

## Environment Variables

To use Azure Key Vault, configure the provider and the URL of the vault:

| Key                         | Value                                                          |
|-----------------------------|----------------------------------------------------------------|
| `SECRET_PROVIDER`           | `azure_kv`                                                     |
| `SECRET_PROVIDER_VAULT_URL` | Full vault URL, for example `https://my-vault.vault.azure.net` |

Azure authentication normally uses [`DefaultAzureCredential`](https://learn.microsoft.com/en-us/azure/developer/go/sdk/authentication/credential-chains#defaultazurecredential-overview).
It supports managed identity, workload identity, and service-principal credentials
through the standard Azure environment variables.

For a service principal, set:

| Key                        | Value                                                          |
|----------------------------|----------------------------------------------------------------|
| `AZURE_TENANT_ID`          | Microsoft Entra tenant ID                                      |
| `AZURE_CLIENT_ID`          | Application/client ID                                          |
| `AZURE_CLIENT_SECRET`      | Application client secret                                      |
| `AZURE_CLIENT_SECRET_FILE` | Path to a file containing the application client secret        |

Set either `AZURE_CLIENT_SECRET` or `AZURE_CLIENT_SECRET_FILE`, not both.

For a user-assigned managed identity, set `AZURE_CLIENT_ID` to the identity's client ID. 
A system-assigned managed identity does not require additional credential environment variables.

!!! tip "Grant read-only access"
    Assign the identity the **Key Vault Secrets User** role on the vault, or an
    equivalent access policy that grants the `secrets/get` permission.

## Deployment configuration

Map each deployment environment variable to a Key Vault secret name:

```yaml title=".doco-cd.yml"
name: myapp
external_secrets:
  DATABASE_PASSWORD: database-password
  API_TOKEN: api-token
```

The latest enabled version of each secret is used by default.

To pin a specific secret version, append the version after a slash:

```yaml title=".doco-cd.yml"
name: myapp
external_secrets:
  DATABASE_PASSWORD: database-password/7c3cc89647ba4b9f8dd7d8d5f92d00b9
```

Secret references must use `name` or `name/version`. Full Azure secret
identifier URLs are not supported.
