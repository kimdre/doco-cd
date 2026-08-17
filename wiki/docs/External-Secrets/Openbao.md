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

For the environment variables that control [automatic certificate rotation](#automatic-certificate-rotation), see below.

## Deployment configuration

Add a mapping/reference between the environment variable you want to set in the docker compose project/stack and the reference to the key-value secret in OpenBao.

By default, the root namespace is used (`root` or `/`), but you can specify a different namespace by adding it as the first part of the reference.

- A valid key-value secret reference should use the syntax: 
  ```
  kv:<namespace(optional)>:<secretEngine>:<secretName>:<key>
  ```
- A valid PKI certificate reference (read-only, fetches an already-issued certificate) should use the syntax:
  ```
  pki:<namespace(optional)>:<secretEngine>:<commonName>
  ```
- A valid PKI role reference (issues a **new** certificate on every deployment/rotation, see [Automatic Certificate Rotation](#automatic-certificate-rotation)) should use the syntax:
  ```
  pki-role:<namespace(optional)>:<secretEngine>:<role>:<commonName>
  ```

Examples of valid references:

- `kv:prod-secrets:db-prod:username` &rarr; Fetches the `username` key from the `db-prod` key-value secret in the `prod-secrets` secret engine in the `root` namespace.
- `kv:root:prod-secrets:db-prod:username` &rarr; Same as above, explicitly specifying the `root` namespace.
- `kv:my-namespace:secret:api-keys:stripe` &rarr; Fetches the `stripe` key from the `api-keys` secret in the `secret` key-value secret engine in the `my-namespace` namespace.
- `pki:certs:myapp.example.com` &rarr; Fetches the certificate for the common name `myapp.example.com` from the `certs` pki secret engine in the `root` namespace.
- `pki:my-namespace:certs:myapp.example.com` &rarr; Fetches the certificate for the common name `myapp.example.com` from the `certs` pki secret engine in the `my-namespace` namespace.
- `pki-role:certs:myapp-role:myapp.example.com` &rarr; Issues a new certificate for the common name `myapp.example.com` using the `myapp-role` PKI role in the `certs` secret engine in the `root` namespace.
- `pki-role:my-namespace:certs:myapp-role:myapp.example.com` &rarr; Same as above, in the `my-namespace` namespace.

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

## Automatic Certificate Rotation

Certificates issued via a `pki-role:` reference (see above) are eligible for **automatic rotation**:
doco-cd runs a background watcher that tracks the expiry of every deployment whose certificate(s) were
all issued through `pki-role:` refs, and automatically reissues the certificate(s) and redeploys the
project before they expire.

!!! note
    Deployments that use the read-only `pki:` reference are **not** eligible for automatic rotation,
    since that reference only reads an already-issued certificate and cannot reissue a new one. Use
    `pki-role:` if you want doco-cd to keep your certificates renewed automatically.

### Enabling automatic rotation

Automatic certificate rotation is currently only supported for OpenBao PKI roles:

| Key                            | Type     | Description                                                                                                                                                  | Default |
|--------------------------------|----------|--------------------------------------------------------------------------------------------------------------------------------------------------------------|---------|
| `CERT_ROTATION_ENABLED`        | boolean  | Enables the built-in automatic certificate rotation watcher.                                                                                                 | `false` |
| `CERT_ROTATION_THRESHOLD`      | duration | How far ahead of a certificate's expiry doco-cd triggers automatic rotation. Accepts a [Go duration](https://pkg.go.dev/time#ParseDuration) (e.g. `72h`).    | `72h`   |
| `CERT_ROTATION_CHECK_INTERVAL` | duration | How often the certificate rotation watcher checks deployed certificates for upcoming expiry. Accepts a [Go duration](https://pkg.go.dev/time#ParseDuration). | `1h`    |

### How it works

1. When a deployment resolves a `pki-role` external secret, doco-cd stamps three labels on the
   deployed container(s)/service(s):
    - `cd.doco.deployment.cert.expiry`: the earliest expiry (RFC3339) among all certificates in the deployment.
    - `cd.doco.deployment.cert.rotatable`: `true` only if **every** certificate-bearing external secret in the deployment used a `pki-role` reference.
    - `cd.doco.deployment.cert.state`: the reference and serial number of every deployed `pki-role` certificate, used to detect revocation.
2. The watcher periodically (every `CERT_ROTATION_CHECK_INTERVAL`) lists all deployments labeled
   `cert.rotatable=true` and rotates a deployment when either is true:
    - its recorded expiry is within `CERT_ROTATION_THRESHOLD` (or already past),
    - or any certificate in its `cert.state` label has since been revoked in OpenBao.
3. When a deployment is due, doco-cd reloads its deploy config and re-resolves all external
   secrets. It issues brand-new certificates and keys for every `pki-role` reference, then
   redeploys to pick up the fresh values:

    Only services that actually consume a rotated certificate or private key (via direct environment variables or `configs`/`secrets` entries) get recreated; unrelated services in the same project are left untouched.

### Private key access

A `pki-role:` reference automatically exposes the certificate's matching private key as a second
environment variable, named after the certificate's variable with a `_KEY` suffix. For example, an
external secret named `CERT` referencing `pki-role:...` resolves to both:

- `CERT` &rarr; the certificate PEM
- `CERT_KEY` &rarr; the matching private key PEM

```yaml title=".doco-cd.yml"
name: myapp
external_secrets:
  CERT: pki-role:pki:myapp-role:myapp.example.com
```

```yaml title="docker-compose.yml"
configs:
  myapp-cert.crt:
    environment: CERT
  myapp-cert.key:
    environment: CERT_KEY

services:
  app:
    image: myapp:latest
    configs:
      - source: myapp-cert.crt
        target: /etc/ssl/certs/myapp.crt
      - source: myapp-cert.key
        target: /etc/ssl/private/myapp.key
```

### Examples

#### mTLS

To secure service-to-service communication with mutual TLS (mTLS), issue a client certificate for
your application through a PKI role dedicated to mTLS clients, and let doco-cd keep it renewed
automatically:

```yaml title=".doco-cd.yml"
name: myapp
external_secrets:
  MTLS_CERT: pki-role:pki:mtls-client-role:myapp.internal
```

```yaml title="docker-compose.yml"
configs:
  myapp-mtls.crt:
    content: $MTLS_CERT
  myapp-mtls.key:
    content: $MTLS_CERT_KEY

services:
  app:
    image: myapp:latest
    configs:
      - source: myapp-mtls.crt
        target: /etc/ssl/certs/mtls-client.crt
      - source: myapp-mtls.key
        target: /etc/ssl/private/mtls-client.key
```

Enable `CERT_ROTATION_ENABLED=true` on the doco-cd instance, and the client certificate will be
reissued and redeployed automatically before it expires — mTLS handshakes never fail due to an
expired client certificate.

#### Private PKI

For internal services that need a server certificate signed by your own private Certificate
Authority, define a PKI role that constrains what common names/domains it may issue for (e.g.
`allowed_domains: internal.example.com`), then reference it the same way:

```yaml title=".doco-cd.yml"
name: internal-service
external_secrets:
  SERVER_CERT: pki-role:pki:internal-services-role:api.internal.example.com
```

```yaml title="docker-compose.yml"
configs:
  server.crt:
    content: $SERVER_CERT
  server.key:
    content: $SERVER_CERT_KEY

services:
  app:
    image: internal-service:latest
    configs:
      - source: server.crt
        target: /etc/ssl/certs/server.crt
      - source: server.key
        target: /etc/ssl/private/server.key
```

With `CERT_ROTATION_ENABLED=true`, doco-cd continuously monitors the certificate's expiry and
automatically reissues + redeploys the service well ahead of expiry, so your internal PKI-secured
services stay trusted without any manual certificate management.
