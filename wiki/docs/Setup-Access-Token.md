---
tags:
  - Setup
  - Configuration
---

# Setup Git Access Token

This page shows how to set up a Git access token for your deployments.

Use this if your repository URL starts with `http://` or `https://`.
The token is used to authenticate with your Git provider (GitHub, GitLab, Bitbucket, etc.) and to clone or fetch your repositories via HTTP.

!!! tip "Usage without Git Access Token"
    You can use doco-cd without a Git Access Token if the repositories you want to use for your deployments are publicly accessible. However, it is still recommended to use one in that case to for example avoid rate limits.

!!! info "About Git Authentication"
    See [Git Authentication](Git-Settings.md#authentication) for more information on how doco-cd handles Git authentication
    and how to set up global and per-domain credentials.

!!! info "Using GitHub Apps"
    If you use GitHub, you can also authenticate using a [GitHub App](Git-Settings.md#github-apps).

## Git Providers

--8<-- "wiki/includes/git-access-token-permissions.md"

## Setting up the Token

After you create the token, pass it to Doco-CD as an environment variable or as a file.
The right way depends on where the Compose file of Doco-CD lives.

Also set [`GIT_AUTH_DOMAINS`](Git-Settings.md#credential-scope) to the hosts that may receive the token.
Doco-CD does not send the token to other hosts, and it does not start if the token has no hosts.
The examples below use `github.com`. Replace it with the host of your Git provider.

=== "Compose file on the Docker host"

    Use this if you keep the Compose file of Doco-CD on the Docker host and not in Git (standalone install).
    
    === "Environment variable"
        The sample `docker-compose.yml` already reads `GIT_ACCESS_TOKEN` from the environment.
        Save the token in a `.env` file next to the Compose file of Doco-CD:
    
        ```ini title=".env"
        GIT_ACCESS_TOKEN=your_read_only_token
        GIT_AUTH_DOMAINS=- domains: [github.com]
        ```
    
        Run `chmod 600 .env` so other users of the host cannot read it. Then start or recreate Doco-CD with `docker compose up -d`.
    
    === "Secret file"
        Doco-CD reads the token from a file if you set `GIT_ACCESS_TOKEN_FILE`. This keeps the token out of the container's environment.
        Save the token in a file that only root can read, then mount it as a [Docker secret](App-Settings.md#usage-with-docker-secrets):
    
        ```sh
        sudo mkdir -p /opt/doco-cd
        echo 'your_read_only_token' > /opt/doco-cd/git-access-token
        sudo chmod 600 /opt/doco-cd/git-access-token
        ```
    
        ```yaml title="docker-compose.yml"
        services:
          app:
            environment:
              GIT_ACCESS_TOKEN_FILE: /run/secrets/git_access_token
              GIT_AUTH_DOMAINS: "- domains: [github.com]" # hosts that receive the token
            secrets:
              - git_access_token
    
        secrets:
          git_access_token:
            file: /opt/doco-cd/git-access-token
        ```
    
        Use either `GIT_ACCESS_TOKEN` or `GIT_ACCESS_TOKEN_FILE`, not both.

=== "Compose file in your Git repository (self-updating)"

    Use this if Doco-CD deploys its own Compose file from your Git repository, as in the
    [self-updating setup](Advanced/Self-Updating.md).
    The examples below use the service name `doco-cd` and the file `doco-cd/compose.yaml` of the
    [self-updating example](https://github.com/kimdre/doco-cd/tree/main/examples/self-updating).
    
    Doco-CD has to clone your repository before it can read anything from it.
    Because of this, the [bootstrap script](Advanced/Self-Updating.md#bootstrap) always needs the token from outside the repository.
    Pass it in your shell and do not save it in a file in the repository.
    After the bootstrap, the running Doco-CD gets the token from the Compose file in your repository in one of these ways:
    
    !!! warning "Do not write/commit the token in plaintext in your Git repository"
        Use [SOPS encryption](Advanced/Encryption.md) or [External Secrets](External-Secrets/index.md) to keep the token secret in your repository.

    === "Secret file on the host"
        The repository contains only the path of a token file on the Docker host, never the token itself.
        Save the token in a file that only root can read:
    
        ```sh
        sudo mkdir -p /opt/doco-cd
        echo 'your_read_only_token' > /opt/doco-cd/git-access-token
        sudo chmod 600 /opt/doco-cd/git-access-token
        ```
    
        Reference the file in the Compose file of Doco-CD in your repository:
    
        ```yaml title="doco-cd/compose.yaml"
        services:
          doco-cd:
            environment:
              GIT_ACCESS_TOKEN_FILE: /run/secrets/git_access_token
              GIT_AUTH_DOMAINS: "- domains: [github.com]" # hosts that receive the token
            secrets:
              - git_access_token
    
        secrets:
          git_access_token:
            file: /opt/doco-cd/git-access-token # Path on the Docker host
        ```
    
        The example's `compose.yaml` already has these lines as comments. Push the change, then run the bootstrap with the token:
    
        ```sh
        export GIT_ACCESS_TOKEN="$(sudo cat /opt/doco-cd/git-access-token)"
        ./bootstrap.sh
        ```
    
    === "SOPS-encrypted file in Git"
        The repository contains the token as a [SOPS](Advanced/Encryption.md)-encrypted file.
        The age key that decrypts it stays on the Docker host.
        This way, one key on the host protects the token and any other secret you keep in Git.
    
        1. Create an age key on the Docker host. The command prints the public key. Note it.
    
            ```sh
            sudo mkdir -p /opt/doco-cd
            sudo age-keygen -o /opt/doco-cd/sops_age_key.txt
            ```
    
        2. On your own computer, encrypt the token with the **public** key and add the result to your repository.
           Use the file extension `.txt` so that SOPS treats the file as plain text.
    
            ```sh
            echo 'your_read_only_token' > git-access-token.txt
            sops encrypt --age <age_public_key> git-access-token.txt > doco-cd/git-access-token.enc.txt
            rm git-access-token.txt
            ```
    
        3. Reference both files in the Compose file of Doco-CD in your repository:
    
            ```yaml title="doco-cd/compose.yaml"
            services:
              doco-cd:
                environment:
                  GIT_ACCESS_TOKEN_FILE: /run/secrets/git_access_token
                  GIT_AUTH_DOMAINS: "- domains: [github.com]" # hosts that receive the token
                  SOPS_AGE_KEY_FILE: /run/secrets/sops_age_key
                secrets:
                  - git_access_token
                  - sops_age_key
    
            secrets:
              git_access_token:
                file: ./git-access-token.enc.txt # Encrypted file in your repository
              sops_age_key:
                file: /opt/doco-cd/sops_age_key.txt # Path on the Docker host
            ```
    
            Doco-CD decrypts the file in its own copy of the repository before it starts the container.
            The container reads the plain token, and your repository keeps only the encrypted file.
    
        4. Add the age key to the `docker run` command in `bootstrap.sh`, so that the bootstrap can decrypt the file as well:
    
            ```sh title="bootstrap.sh"
              -e SOPS_AGE_KEY="${SOPS_AGE_KEY:-}" \
            ```
    
        5. Push your changes, then run the bootstrap with the token and the age key:
    
            ```sh
            export GIT_ACCESS_TOKEN=<your_git_access_token>
            export SOPS_AGE_KEY="$(sudo cat /opt/doco-cd/sops_age_key.txt)"
            ./bootstrap.sh
            ```
    
            If Docker needs `sudo` on your host, run `sudo --preserve-env=GIT_ACCESS_TOKEN,SOPS_AGE_KEY ./bootstrap.sh` instead.
    
        !!! tip
            Back up the age key. Without it, Doco-CD cannot decrypt the token and cannot update itself.
    
    === "External secret provider"
        The repository contains only a reference to the token in an [external secret provider](External-Secrets/index.md).
        Doco-CD fetches the token during each deployment of its own Compose file.
        You still need to give Doco-CD the access token of the provider, for example from a file on the Docker host.
        This example uses [Bitwarden Secrets Manager](External-Secrets/Bitwarden-Secrets-Manager.md).
        The settings for other providers are on their own pages.
    
        Reference the token in the deployment configuration of Doco-CD:
    
        ```yaml title=".doco-cd.yml" hl_lines="5-6"
        name: doco-cd
        working_dir: doco-cd
        compose_files: [compose.yaml]
        timeout: 120
        external_secrets:
          GIT_ACCESS_TOKEN: 138e3a97-ed58-431c-b366-b35500663411 # ID of the secret in your provider
        ```
    
        Use the variable in the Compose file of Doco-CD:
    
        ```yaml title="doco-cd/compose.yaml"
        services:
          doco-cd:
            environment:
              GIT_ACCESS_TOKEN: ${GIT_ACCESS_TOKEN}
              GIT_AUTH_DOMAINS: "- domains: [github.com]" # hosts that receive the token
              SECRET_PROVIDER: bitwarden_sm
              SECRET_PROVIDER_ACCESS_TOKEN_FILE: /run/secrets/secret_provider_access_token
            secrets:
              - secret_provider_access_token
    
        secrets:
          secret_provider_access_token:
            file: /opt/doco-cd/secret-provider-access-token # Path on the Docker host
        ```
    
        The bootstrap needs the provider settings and the token to clone the repository.
        Add the provider settings to the `docker run` command in `bootstrap.sh`:
    
        ```sh title="bootstrap.sh"
          -e SECRET_PROVIDER=bitwarden_sm \
          -e SECRET_PROVIDER_ACCESS_TOKEN="${SECRET_PROVIDER_ACCESS_TOKEN:-}" \
        ```
    
        Then run the bootstrap:
    
        ```sh
        export GIT_ACCESS_TOKEN=<your_git_access_token>
        export SECRET_PROVIDER_ACCESS_TOKEN="$(sudo cat /opt/doco-cd/secret-provider-access-token)"
        ./bootstrap.sh
        ```
    
    Do not set `GIT_ACCESS_TOKEN` and `GIT_ACCESS_TOKEN_FILE` in the same container. Doco-CD refuses to start if both are set.
    
    Some providers need a specific username together with the token. Set `GIT_ACCESS_TOKEN_USER` in that case; see [Git Authentication](Git-Settings.md#authentication).
