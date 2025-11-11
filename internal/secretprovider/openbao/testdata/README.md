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

8. Enable PKI engine
    ```bash
    docker exec -it openbao-dev-vault-1 sh -c "vault secrets enable pki"
    ```
   
9. Create a root certificate
    ```bash
    docker exec -it openbao-dev-vault-1 sh -c "vault write pki/root/generate/internal common_name='example.internal' ttl=8760h"
    ```
   
10. Create a role for issuing certificates
    ```bash
    docker exec -it openbao-dev-vault-1 sh -c "vault write pki/roles/example-dot-internal allowed_domains='example.internal' allow_subdomains=true max_ttl=72h"
    ```
    
11. Issue a test certificate
    ```bash
    docker exec -it openbao-dev-vault-1 sh -c "vault write pki/issue/example-dot-internal common_name='test.example.internal' ttl=24h"
    ```