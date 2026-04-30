---
tags:
  - Advanced
  - Secrets
  - Configuration
---

# Webhook

The webhook provider uses global [secret stores](Webhook.md#secret-store) defined in YAML.
Each secret reference in `.doco-cd.yml` points to one store via `store_ref` and passes input values via `remote_ref`.

## Environment Variables

To use it, set the following environment variables:

| Key                                   | Value                                                                                      |
|---------------------------------------|--------------------------------------------------------------------------------------------|
| `SECRET_PROVIDER`                     | `webhook`                                                                                  |
| `SECRET_PROVIDER_WEBHOOK_STORES`      | YAML content defining webhook stores (mutually exclusive with `..._FILE`)                  |
| `SECRET_PROVIDER_WEBHOOK_STORES_FILE` | Path to a YAML file containing the store definitions (mutually exclusive with `...STORES`) |
| `SECRET_PROVIDER_AUTH_USERNAME`       | Optional auth value exposed in templates as `{{ .auth.username }}`                         |
| `SECRET_PROVIDER_AUTH_PASSWORD`       | Optional auth value exposed in templates as `{{ .auth.password }}`                         |
| `SECRET_PROVIDER_AUTH_TOKEN`          | Optional auth value exposed in templates as `{{ .auth.token }}`                            |
| `SECRET_PROVIDER_AUTH_APIKEY`         | Optional auth value exposed in templates as `{{ .auth.api_key }}`                          |

## Secret Store

### Format

A store supports the following fields:

| Field       | Required | Description                                                                          |
|-------------|----------|--------------------------------------------------------------------------------------|
| `name`      | Yes      | Store name (must be unique).                                                         |
| `version`   | Yes      | Store schema version (currently `v1`).                                               |
| `url`       | Yes      | Request URL template.                                                                |
| `json_path` | Yes      | [JMESPath](https://jmespath.org/) expression used to extract the final secret value. |
| `method`    | No       | HTTP method. Defaults to `GET`.                                                      |
| `headers`   | No       | Optional HTTP headers map (template-supported).                                      |
| `body`      | No       | Optional HTTP request body template.                                                 |

!!! warning "The provider fails fast when:"

    - `store_ref` does not exist
    - a referenced `remote_ref` field is missing
    - `json_path` is missing or renders empty

!!! example "Examples for `json_path`"
  
    - `data.login.password`
    - `data.fields[?name=='password'].value`

### Example store definitions

The webhook provider supports two definition styles:

- `#!yaml stores:` map style (multiple named stores in one document)
- top-level single-store style (can be combined using YAML multi-document `#!yaml ---`)

=== "`#!yaml stores:` map style"

    ```yaml title="secret-stores.yml"
    stores:
      bitwarden-login:
        version: v1
        url: "http://bitwarden-api:8087/object/item/{{ .remote_ref.key }}"
        method: GET
        headers:
          Content-Type: application/json
        json_path: "data.login.{{ .remote_ref.property }}"

      bitwarden-fields:
        version: v1
        url: "http://bitwarden-api:8087/object/item/{{ .remote_ref.key }}"
        method: GET
        json_path: "data.fields[?name=='{{ .remote_ref.property }}'].value"
    ```

=== "Single-store multi-document style"

    ```yaml title="secret-stores.yml"
    name: bitwarden-login
    version: v1
    url: "http://bitwarden-api:8087/object/item/{{ .remote_ref.key }}"
    method: GET
    headers:
      Content-Type: application/json
    json_path: "data.login.{{ .remote_ref.property }}"
    ---
    name: akeyless
    version: v1
    url: "https://api.akeyless.io/v2/get-secret-value"
    method: POST
    headers:
      Content-Type: application/json
      Authorization: "Basic {{ print .auth.username \":\" .auth.password | b64enc }}"
    body: '{"secret_name":"{{ .remote_ref.key }}","auth_method_access_token":"{{ .auth.token }}"}'
    json_path: "value"
    ```

## Deployment Configuration

For webhook, `external_secrets` entries must use object references.
Legacy string refs (e.g. `#!yaml DB_PASSWORD: some-id`) are rejected with a clear error.

```yaml title=".doco-cd.yml"
name: myapp
external_secrets:
  DB_USERNAME:
    store_ref: bitwarden-login
    remote_ref:
      key: 12345678-aaaa-bbbb-cccc-123456789abc
      property: username

  DB_PASSWORD:
    store_ref: bitwarden-login
    remote_ref:
      key: 12345678-aaaa-bbbb-cccc-123456789abc
      property: password

  API_KEY:
    store_ref: bitwarden-fields
    remote_ref:
      key: dddddddd-1111-2222-3333-eeeeeeeeeeee
      property: api_key
```

## Template Parameters

All templated store fields (`url`, `headers`, `body`, `json_path`) can access the following objects:

| Parameter    | Description                                                                                                                                                            |
|--------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `remote_ref` | The per-secret object from `.doco-cd.yml` under `external_secrets.<NAME>.remote_ref`.</br>You can define any keys (for example `key`, `property`, `query`, `filters`). |
| `auth`       | Provider auth values from `SECRET_PROVIDER_AUTH_*` environment variables.</br>Available keys: `username`, `password`, `token`, `api_key`.                              |

!!! example

    ```yaml title=".doco-cd.yml"
    external_secrets:
      DB_PASSWORD:
        store_ref: bitwarden-login
        remote_ref:
          key: 12345678-aaaa-bbbb-cccc-123456789abc
          property: password
    ```
    
    ```yaml title="store definition"
    url: "http://bitwarden-api:8087/object/item/{{ .remote_ref.key }}"
    json_path: "data.login.{{ .remote_ref.property }}"
    headers:
      Authorization: "Bearer {{ .auth.token }}"
    ```
    
    Rendered values (example):
    
    - `url`: `http://bitwarden-api:8087/object/item/12345678-aaaa-bbbb-cccc-123456789abc`
    - `json_path`: `data.login.password`

## Template Functions

Template functions are available in all templated fields (`url`, `headers`, `body`, `json_path`).
Functions can be chained with `|` from left to right.

| Function    | Purpose                                |
|-------------|----------------------------------------|
| `b64enc`    | Base64-encode a value                  |
| `b64dec`    | Base64-decode a value                  |
| `urlencode` | URL-encode a value                     |
| `urldecode` | URL-decode a value                     |
| `json`      | Convert a value to a JSON string       |
| `toUpper`   | Convert text to uppercase              |
| `toLower`   | Convert text to lowercase              |
| `trim`      | Remove leading and trailing whitespace |

??? example "Function examples"

    - `{{ "secret123" | b64enc }}` -> `c2VjcmV0MTIz`
    - `{{ "c2VjcmV0MTIz" | b64dec }}` -> `secret123`
    - `{{ "hello world" | urlencode }}` -> `hello+world`
    - `{{ "hello+world" | urldecode }}` -> `hello world`
    - `{{ .remote_ref.data | json }}` -> `{"key":"value"}`
    - `{{ "hello" | toUpper }}` -> `HELLO`
    - `{{ "HELLO" | toLower }}` -> `hello`
    - `{{ "  hello  " | trim }}` -> `hello`

## Examples

### Basic Authentication Header
```yaml
headers:
  Authorization: "Basic {{ print .auth.username \":\" .auth.password | b64enc }}"
```
With `auth.username=admin` and `auth.password=secret123`:

- Result: `Authorization: Basic YWRtaW46c2VjcmV0MTIz`

### URL-encoded Query Parameter
```yaml
url: "https://api.example.com/search?q={{ .remote_ref.query | urlencode }}"
```
With `remote_ref.query=hello world`:

- Result: `https://api.example.com/search?q=hello+world`

### JSON Request Body
```yaml
body: '{"filters":{{ .remote_ref.filters | json }}}'
```

With `remote_ref` containing a map of filters:

```yaml
remote_ref:
  filters:
    status: active
    type: user
```

- Result: `{"filters":{"status":"active","type":"user"}}`

### Trimmed and Uppercase API Key Header
```yaml
headers:
  X-API-Key: "{{ .remote_ref.api_key | trim | toUpper }}"
```
With `remote_ref.api_key=<space>my-secret-key<space>`:

- Result: `X-API-Key: MY-SECRET-KEY`

### Decoded and Trimmed Secret
```yaml
json_path: "secret[?key=='{{ .remote_ref.encoded_id | b64dec | trim }}']"
```
With `remote_ref.encoded_id=c2VjcmV0LWtleQ==`:

- Result: `secret[?key=='secret-key']`
