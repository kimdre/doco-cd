# Bitwarden Vault / Vaultwarden


This section describes how to integrate Doco-CD with Bitwarden Vault or self-hosted Vaultwarden to securely manage and inject secrets during deployments.

### How It Works

Doco-CD uses the [webhook provider](webhook.md) together with a lightweight Bitwarden REST API sidecar container that exposes your vault items over HTTP. The sidecar securely fetches secrets from Bitwarden/Vaultwarden and makes them available to Doco-CD for injection into your Docker Compose configurations.

!!! tip
    This approach is fully compatible with both cloud-hosted Bitwarden Vault and self-hosted Vaultwarden instances.


### Architecture Overview

The integration follows this workflow:

1. **Sidecar Service**: Run the [`ghcr.io/kimdre/bitwarden-rest-api-server`](https://github.com/kimdre/bitwarden-rest-api-server) container alongside Doco-CD
2. **Store Configuration**: Define webhook [secret stores](webhook.md#secret-store) in a YAML file (`SECRET_PROVIDER_WEBHOOK_STORES_FILE`) that specify how to query items from the [Bitwarden Vault Management API](https://bitwarden.com/help/vault-management-api/)
3. **Secret References**: In your `.doco-cd.yml`, reference vault items using `store_ref` and `remote_ref` to retrieve specific secrets

### Prerequisites

- A Bitwarden Vault account or self-hosted Vaultwarden instance
- Personal API Key (OAuth2 credentials) from your Bitwarden account or organization (see [Bitwarden Personal API Key](https://bitwarden.com/help/personal-api-key/))
- Access to your Bitwarden item UUIDs (see [Finding Item UUIDs](#finding-item-uuids) below)

!!! note
    The compose example below is not meant to replace your full Doco-CD compose file.
    Instead, treat it as the additional services, environment variables, volumes, and dependencies you need to add to your existing basic Doco-CD compose setup.

    You can for example do this by creating a separate `bitwarden-vault.compose.yml` file and then use it together with your main compose file during deployment:

    ```bash
    docker compose -f docker-compose.yml -f bitwarden-vault.compose.yml up -d
    ```


### Setup Steps

#### Example compose setup

```yaml
services:
  app: # doco-cd
    environment:
      SECRET_PROVIDER: webhook
      SECRET_PROVIDER_WEBHOOK_STORES_FILE: /secret-store.yml
    volumes:
      - ./secret-store.yml:/secret-store.yml:ro
    depends_on:
      bitwarden-api:
        condition: service_healthy
  
  bitwarden-api: # sidecar that provides the Bitwarden Vault Management API
    image: ghcr.io/kimdre/bitwarden-rest-api-server:latest
    environment:
      # Set these environment variables or use a .env file, see the image docs for more configuration options
      # Refer to the image documentation for the configuration options: https://github.com/kimdre/bitwarden-rest-api-server#getting-started
      - BW_HOST=${BW_HOST:-https://vault.bitwarden.com}
      - BW_CLIENTID=${BW_CLIENTID:?error}
      - BW_CLIENTSECRET=${BW_CLIENTSECRET:?error}
      - BW_PASSWORD=${BW_PASSWORD:?error}
    expose: # this makes the port only available inside the doco-cd compose project instead of on the entire host
      - "8087"
    restart: unless-stopped
    init: true
    read_only: true
    cap_drop:
      - ALL
    volumes:
      - bw-data:/data
    depends_on:
      set_permissions:
        condition: "service_completed_successfully"
        
  set_permissions: # required for correct bitwarden-api volume permissions 
    image: busybox:latest
    command: sh -c "chown -R 65532:65532 /data"
    volumes:
      - bw-data:/data
    restart: "no"

volumes:
  bw-data:
```

**Environment variables:**

!!! note
    For all configuration options, refer to the image documentation at https://github.com/kimdre/bitwarden-rest-api-server#getting-started.


- `BW_HOST` (optional) - Bitwarden or Vaultwarden API host. Defaults to `https://vault.bitwarden.com`.  
  For Vaultwarden, use your self-hosted instance URL (e.g., `https://vault.example.com`)
- `BW_CLIENTID` (required) - Client ID from your personal API Key credentials
- `BW_CLIENTSECRET` (required) - Client Secret from your personal API Key credentials
- `BW_PASSWORD` (required) - Master password for your Bitwarden account

Store these values in a `.env` file or set them inside the compose file.

For detailed information on setting up your personal API Key, see: https://bitwarden.com/help/personal-api-key/

#### Finding Item UUIDs

To reference secrets from Bitwarden in your `.doco-cd.yml`, you need the UUID of the Bitwarden item (vault entry).

**Using Bitwarden CLI:**

```bash
# Login to Bitwarden (if not already logged in)
bw login your-email@example.com

# List all items in your vault
bw list items

# The output will show each item with its id (UUID)
```

Example CLI output:

```json
[
  {
    "object": "item",
    "id": "12345678-aaaa-bbbb-cccc-123456789abc",
    "name": "Database Credentials",
    "login": {
      "username": "dbuser",
      "password": "mysecretpassword"
    },
    "fields": [
      {
        "type": 0,
        "name": "api_key",
        "value": "sk-1234567890"
      }
    ]
  }
]
```

**Using Bitwarden Web Vault:**

1. Open your Bitwarden vault at https://vault.bitwarden.com (or your Vaultwarden URL)
2. Click on an item to view its details
3. The UUID can be found in the URL (`itemId=<UUID>`)

#### Example webhook store file for Bitwarden Vault

```yaml
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

In this example:
- `bitwarden-login` fetches built-in login fields such as `username` and `password`
- `bitwarden-fields` fetches custom fields by name

#### Minimal `.doco-cd.yml` example

```yaml
name: myapp
external_secrets:
  DB_PASSWORD:
    store_ref: bitwarden-login
    remote_ref:
      key: 12345678-aaaa-bbbb-cccc-123456789abc
      property: password
```

#### Extended `.doco-cd.yml` example

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

With this setup, Doco-CD resolves the secret value by:
1. rendering the configured store templates using `remote_ref`
2. calling the Bitwarden sidecar over HTTP
3. extracting the value from the JSON response with `json_path`
