---
tags:
  - External Secrets
  - Configuration
---

# Webhook

The webhook provider uses global [secret stores](webhook.md#secret-store) defined in YAML.
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

A store must define:

- `name`
- `version` (currently `v1`)
- `url`
- `json_path`

And can optionally define:

- `method` (defaults to `GET`)
- `headers`
- `body`

`json_path` expressions use [JMESPath](https://jmespath.org/) syntax (for example `data.login.password` and `data.fields[?name=='password'].value`).

#### Example: map/list schema (`stores:`) and multi-document support

```yaml title="Example Secret Stores (e.g. secret-stores.yml)"
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
Legacy string refs (e.g. `DB_PASSWORD: some-id`) are rejected with a clear error.

```yaml
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

All store templates (`url`, `headers`, `body`, `json_path`) can use:

| Key          | Description                                                                                                                              |
|--------------|------------------------------------------------------------------------------------------------------------------------------------------|
| `remote_ref` | The object provided for that secret in `.doco-cd.yml`.<br/>Can contain any fields, for example `key` and `property` in the example above |
| `auth`       | Provider auth values from `SECRET_PROVIDER_AUTH_*` env vars                                                                              |

## Template Functions

The following functions are available for use in all template fields:

| Function    | Description                  | Input            | Template                           | Result            |
|-------------|------------------------------|------------------|------------------------------------|-------------------|
| `b64enc`    | Encode input to base64       | `secret123`      | `{{ "secret123" \| b64enc }}`      | `c2VjcmV0MTIz`    |
| `b64dec`    | Decode base64 input          | `c2VjcmV0MTIz`   | `{{ "c2VjcmV0MTIz" \| b64dec }}`   | `secret123`       |
| `urlencode` | URL encode input             | `hello world`    | `{{ "hello world" \| urlencode }}` | `hello+world`     |
| `urldecode` | URL decode input             | `hello+world`    | `{{ "hello+world" \| urldecode }}` | `hello world`     |
| `json`      | Convert input to JSON string | `map[key:value]` | `{{ .remote_ref.data \| json }}`   | `{"key":"value"}` |
| `toUpper`   | Convert input to uppercase   | `hello`          | `{{ "hello" \| toUpper }}`         | `HELLO`           |
| `toLower`   | Convert input to lowercase   | `HELLO`          | `{{ "HELLO" \| toLower }}`         | `hello`           |
| `trim`      | Trim whitespace from input   | `  hello  `      | `{{ "  hello  " \| trim }}`        | `hello`           |

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

!!! warning
    The provider fails fast when:

    - `store_ref` does not exist
    - a referenced `remote_ref` field is missing
    - `json_path` is missing or renders empty

