---
tags:
  - Advanced
  - Secrets
  - Configuration
---

# OpenBao

## Environment Variables

To use OpenBao, you need to set the following environment variables:

| Key                                 | Value                                                           |
|-------------------------------------|-----------------------------------------------------------------|
| `SECRET_PROVIDER`                   | `openbao`                                                       |
| `SECRET_PROVIDER_SITE_URL`          | The URL of the OpenBao instance                                 |
| `SECRET_PROVIDER_ACCESS_TOKEN`      | Access token for authenticating with the secret provider        |
| `SECRET_PROVIDER_ACCESS_TOKEN_FILE` | Path to a file containing the access token inside the container |

## Deployment configuration

Add a mapping/reference between the environment variable you want to set in the docker compose project/stack and the reference to the key-value secret in OpenBao.

By default, the root namespace is used (`root` or `/`), but you can specify a different namespace by adding it as the first part of the reference.

- A valid key-value secret reference should use the syntax: 
  ```
  kv:<namespace(optional)>:<secretEngine>:<secretName>:<key>
  ```
- A valid PKI certificate reference should use the syntax:
  ```
  pki:<namespace(optional)>:<secretEngine>:<commonName>
  ```

Examples of valid references:

- `kv:prod-secrets:db-prod:username` &rarr; Fetches the `username` key from the `db-prod` key-value secret in the `prod-secrets` secret engine in the `root` namespace.
- `kv:root:prod-secrets:db-prod:username` &rarr; Same as above, explicitly specifying the `root` namespace.
- `kv:my-namespace:secret:api-keys:stripe` &rarr; Fetches the `stripe` key from the `api-keys` secret in the `secret` key-value secret engine in the `my-namespace` namespace.
- `pki:certs:myapp.example.com` &rarr; Fetches the certificate for the common name `myapp.example.com` from the `certs` pki secret engine in the `root` namespace.
- `pki:my-namespace:certs:myapp.example.com` &rarr; Fetches the certificate for the common name `myapp.example.com` from the `certs` pki secret engine in the `my-namespace` namespace.

### Example

For example in your `.doco-cd.yml`:

```yaml title=".doco-cd.yml"
name: myapp
external_secrets:
  DB_USERNAME: kv:secret:db-prod:username
  DB_PASSWORD: kv:secret:db-prod:password
  CERT: pki:pki:myapp.example.com
```

To use the certificate in your compose file, you can pass the value to a compose config:

```yaml title="docker-compose.yml"
configs:
  myapp-example-com.crt:
    #environment: CERT  # Either pass the variable via the environment like this (without a $ sign)
    content: $CERT  # Or use the content field to directly inject the variable value to the config content

services:
  app:
    image: myapp:latest
    environment:
      DB_USERNAME: $DB_USERNAME
      DB_PASSWORD: $DB_PASSWORD
    configs:
      - source: myapp-example-com.crt
        target: /etc/ssl/certs/example.crt
```
