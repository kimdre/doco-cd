---
tags:
  - Setup
  - Advanced
  - Deployment
  - Secrets
---

# Encryption with SOPS

Doco-CD supports the encryption of sensitive data in your doco-cd app config and deployment files with [SOPS](https://getsops.io/).

## Supported file formats

SOPS supports files in the following formats:

- YAML files with the `.yaml` or `.yml` extension
- JSON files with the `.json` extension
- Dotenv files with the `.env` extension
- Ini files with the `.ini` extension
- Binary/text files (default format)

## Usage with SOPS and age

!!! note
    I recommend to use [SOPS with age](https://getsops.io/docs/#encrypting-using-age) for encrypting your deployment files.

For this, you need to 

1. [Install age](https://github.com/FiloSottile/age?tab=readme-ov-file#installation) on your system 
2. Create an age key pair.
   ```sh
   age-keygen -o sops_age_key.txt
   ```
3. Encrypt your files with SOPS using the age **public** key, see [SOPS: Encrypting using age](https://getsops.io/docs/#encrypting-using-age).
    ```shell
    sops encrypt --age <age_public_key> test.yaml > test.enc.yaml
    ```
4. Set one of the following environment variables below for doco-cd to use the age key with SOPS:
  
    | Key                 | Type   | Description                                                                                            |
    |---------------------|--------|--------------------------------------------------------------------------------------------------------|
    | `SOPS_AGE_KEY`      | string | The age **secret** key (See the [SOPS docs](https://getsops.io/docs/#encrypting-using-age))            |
    | `SOPS_AGE_KEY_FILE` | string | The path inside the container to the file containing the age **secret** key (e.g. `/sops_age_key.txt`) |

    I recommend using the `SOPS_AGE_KEY_FILE` environment variable and mount the age secret key as a Docker secret.
    See the [example below](#doco-cd-configuration) for how to do this.

    !!! info 
        More different options like using SOPS with PGP/GPG can be found in the [SOPS documentation](https://getsops.io/docs/).

5. When triggering a deployment, doco-cd will automatically detect the SOPS-encrypted files and decrypt them using the provided age key.  
   It is important that you give your files the correct file extension, so that the correct file format is used during the decryption process.

!!! tip
    You can also encrypt only parts of a file and keep the rest in plaintext.
    See [Encrypting only parts of a file](https://getsops.io/docs/#encrypting-only-parts-of-a-file) in the SOPS docs for more information.


## Example setup with SOPS and age

### Doco-CD configuration

Example of a `docker-compose.yml` file using SOPS with age:

Use the [docker-compose.yml](https://github.com/kimdre/doco-cd/blob/main/docker-compose.yml) as the base reference and add the following lines to it:

```yaml title="docker-compose.yml" hl_lines="3-6 8-11"
services:
  app:
    environment:
      SOPS_AGE_KEY_FILE: /run/secrets/sops_age_key # (1)!
    secrets:
      - sops_age_key

secrets:
  sops_age_key:
    file: sops_age_key.txt
```

1. docker secrets are always mounted in the `/run/secrets/` directory if no target is specified

### App configuration with SOPS-encrypted values

To use encrypted values in the doco-cd app configuration, store secrets in encrypted text files and reference them with
`*_FILE` environment variables (for example, `GIT_ACCESS_TOKEN_FILE`).
Each variable should point to the encrypted file path inside the container.

For example, to use an encrypted Git access token, create a text file with the token and encrypt it with SOPS:
```bash
printf "my-git-access-token" > git-access-token.txt
sops encrypt --age age1g3lcl... git-access-token.txt > git-access-token.enc.txt
```

Then set the `GIT_ACCESS_TOKEN_FILE` environment variable in your `docker-compose.yml` file to the encrypted file path:

```yaml title="docker-compose.yml" hl_lines="3-6 8-11"
services:
  app:
    environment:
      GIT_ACCESS_TOKEN_FILE: /path/to/git-access-token
    secrets:
      - git_access_token
 
secrets:
  git_access_token:
    file: git-access-token.enc.txt
```

### Deployment with a SOPS-encrypted file

First, I use my age public key from the previously generated key pair to encrypt my `secrets.env` file:

```dotenv title="secrets.env"
DB_PASSWORD=some-secret-password
```

Generate the encrypted file with SOPS:

```sh
sops encrypt --age age1g3lcl... secrets.env > secrets.enc.env
```

!!! tip
    You can later edit the encrypted file in-place with 
    ```sh
    sops edit secrets.enc.env
    ```

Then, I set the encrypted file in my `docker-compose.yml` file:

```yaml title="docker-compose.yml"
services:
  app:
    env_file:
      - secrets.enc.env
```

When I now trigger a deployment, doco-cd will automatically decrypt the `secrets.enc.env` file using the provided age key 
and deploy the container with the environment variables in it.