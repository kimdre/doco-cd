# Dev env for OpenBao Secret Provider

1. Start a local instance of OpenBao
    ```bash
    docker compose up -d
    ```

2. Initialize the Vault
    ```bash
    docker exec -it openbao-dev-vault-1 sh -c "vault operator init -key-shares=1 -key-threshold=1 -format=json > /init/init.json"
    ```

3. Unseal the Vault
    ```bash
    UNSEAL_KEY=$(docker exec -it openbao-dev-vault-1 sh -c "cat /init/init.json" | jq -r '.unseal_keys_b64[0]')
    docker exec -it openbao-dev-vault-1 sh -c "vault operator unseal $UNSEAL_KEY"
    ```

4. Login to the Vault
    ```bash
    ROOT_TOKEN=$(docker exec -it openbao-dev-vault-1 sh -c  "cat /init/init.json" | jq -r '.root_token')
    docker exec -it openbao-dev-vault-1 sh -c "vault login $ROOT_TOKEN"
    ```

5. Enable KV secret engine
    ```bash
    docker exec -it openbao-dev-vault-1 sh -c "vault secrets enable -path=secret kv-v2"
    ```

6. Add a test secret
    ```bash
    docker exec -it openbao-dev-vault-1 sh -c "vault kv put secret/mysecret username='admin' password='password123'"
    ```

7. Verify the secret
    ```bash
    docker exec -it openbao-dev-vault-1 sh -c "vault kv get secret/mysecret"
    ```