---
tags:
  - Setup
  - Advanced
  - Deployment
  - Secrets
---

# Encryption with SOPS

Doco-CD supports the encryption of sensitive data in your doco-cd app config and deployment files with [SOPS](https://getsops.io/).
age is the recommended key service, AWS KMS and other cloud key services work as well, see [below](#usage-with-sops-and-cloud-key-services).

## How doco-cd detects and decrypts encrypted files

When a deployment is triggered, doco-cd decrypts every SOPS-encrypted file in the source revision it just fetched, before the stack is deployed.
Decryption happens in doco-cd's own copy of the source, never in your repository.

Files and directories matched by `.gitignore` are skipped.
The following directories are always excluded: `.git`, `.github`, `.vscode`, `.idea`, and `node_modules`.

An encrypted file whose content did not change since an earlier revision takes the plaintext of that revision, as long as it is still present in the [artifact storage](../Reference/Artifact-Storage.md).
The key service is therefore only called for files that actually changed, which keeps the cost of a cloud key service low.

If doco-cd has no key for a file, it's left encrypted and logged instead of failing the entire deployment.
This is intentional: a repository may legitimately contain secrets encrypted for someone else.
If a deployment actually uses an encrypted file, the deployment fails at that point, naming the file.

Files a compose project references outside the repository (for example a bind mount pointing at an arbitrary host path) are decrypted as well,
when the project is loaded. For bind-mounted directories, all files inside are scanned recursively.

Detection is content-based: a file is treated as SOPS-encrypted if its content contains both `sops` and `ENC[`. No special file naming convention is required.

The format used for decryption is determined by file extension:

## Supported file formats

SOPS supports files in the following formats:

| Format      | Required file extension                             | Example                                    |
|-------------|-----------------------------------------------------|--------------------------------------------|
| YAML        | `.yaml` or `.yml`                                   | `example.yaml`                             |
| JSON        | `.json`                                             | `example.json`                             |
| Dotenv      | `.env`                                              | `example.env`                              |
| INI         | `.ini`                                              | `example.ini`                              |
| Binary/Text | _any other or none_</br>**Fallback/Default format** | `example.txt`</br>`example` (no extension) |

Getting the extension wrong won't prevent detection, but it will cause decryption to fail, so make sure encrypted files have the correct extension for their format.

## Usage with SOPS and age

!!! tip "I recommend to use [SOPS with age](https://getsops.io/docs/#encrypting-using-age) for encrypting your deployment files."

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
        For all available SOPS environment variables and configuration options, see the [SOPS documentation](https://getsops.io/docs/).

5. When triggering a deployment, doco-cd will automatically detect the SOPS-encrypted files and decrypt them using the provided age key.  
   It is important that you give your files the correct file extension, so that the correct file format is used during the decryption process.

!!! tip
    You can also encrypt only parts of a file and keep the rest in plaintext.
    See [Encrypting only parts of a file](https://getsops.io/docs/#encrypting-only-parts-of-a-file) in the SOPS docs for more information.


## Usage with SOPS and cloud key services

doco-cd uses the SOPS Go library, so decryption is not limited to age.
SOPS reads the key metadata from each encrypted file and picks the matching key service, doco-cd needs no configuration for this.
What matters is how the credentials for that key service reach the doco-cd container: only environment variables, mounted files and the instance metadata service of the cloud VM are available.
There is no interactive login (`aws sso login`, `gcloud auth`, `az login`) inside the container.

!!! warning "At least one `SOPS_*` environment variable must be set"
    doco-cd only decrypts when a non-empty environment variable starting with `SOPS_` is set.
    Cloud key services don't need one for decryption itself, so set one as a marker, e.g. `SOPS_KMS_ARN`.
    Without it every deployment that uses an encrypted file fails with `SOPS secret key is not set`.

| Key service                                                                          | Credentials for doco-cd                                                                                                                                                        | Notes                                                                                  |
|--------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------|
| [age](https://getsops.io/docs/usage/identities/age/)                                 | `SOPS_AGE_KEY` or `SOPS_AGE_KEY_FILE`                                                                                                                                          | See [above](#usage-with-sops-and-age)                                                  |
| [AWS KMS](https://getsops.io/docs/usage/identities/amazon-aws-kms/)                  | EC2 instance profile via IMDS, or `AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY`, or a mounted credentials file via `AWS_SHARED_CREDENTIALS_FILE`                               | `AWS_REGION` is required, see [example below](#example-with-aws-kms). Verified.        |
| [GCP KMS](https://getsops.io/docs/usage/identities/google-cloud-kms/)                | Service account attached to the VM via the metadata server, or a service account key via `GOOGLE_CREDENTIALS` (JSON content or path) / `GOOGLE_APPLICATION_CREDENTIALS` (path) | Not verified with doco-cd, follows from the SOPS credential chain                      |
| [Azure Key Vault](https://getsops.io/docs/usage/identities/azure-kms/)               | Managed identity of the VM via IMDS, or a service principal via `AZURE_TENANT_ID` + `AZURE_CLIENT_ID` + `AZURE_CLIENT_SECRET`                                                  | Not verified with doco-cd, follows from the SOPS credential chain                      |
| [HashiCorp Vault](https://getsops.io/docs/usage/identities/hashicorp-vault-openbao/) | `VAULT_TOKEN` (the Vault address is stored in the encrypted file)                                                                                                              | Token must be long-lived or renewed outside doco-cd. Not verified with doco-cd.        |
| [PGP](https://getsops.io/docs/usage/identities/pgp/)                                 | Legacy `secring.gpg` with an unprotected key, mounted under `GNUPGHOME`                                                                                                        | Not recommended: the image has no `gpg` binary, so only the pure Go OpenPGP path works |

Several recipients on one file are fine, e.g. KMS for humans and CI plus an age key for doco-cd.

### Example with AWS KMS

1. Encrypt your files with SOPS using the KMS key:
    ```shell
    sops encrypt --kms arn:aws:kms:eu-central-1:123456789012:key/<key_id> test.yaml > test.enc.yaml
    ```
2. Give the doco-cd host credentials that allow `kms:Decrypt` on that key.
   An EC2 instance profile (IAM role) is the least effort, static credentials via `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` work as well.
3. Set the environment variables:

    ```yaml title="docker-compose.yml" hl_lines="3-5"
    services:
      app:
        environment:
          AWS_REGION: eu-central-1 # (1)!
          SOPS_KMS_ARN: arn:aws:kms:eu-central-1:123456789012:key/<key_id> # (2)!
    ```

    1. Required. The AWS SDK doesn't discover the region from instance metadata.
    2. Only the marker described above, the actual key ARN comes from the encrypted file.

!!! note "Instance profile from inside a container"
    The container reaches the instance metadata service through the Docker bridge network, which is one extra network hop.
    Set the hop limit of the instance to `2`, otherwise every decryption fails with a credentials error:
    ```shell
    aws ec2 modify-instance-metadata-options --instance-id <id> --http-put-response-hop-limit 2
    ```

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

1. Docker [Secrets](https://docs.docker.com/reference/compose-file/services/#secrets) are always mounted in the `/run/secrets/` directory if no target is specified

### App configuration with SOPS-encrypted values

To use encrypted values in the doco-cd app configuration, store secrets in encrypted text files and reference them with
`*_FILE` environment variables (for example, `GIT_ACCESS_TOKEN_FILE`).
Each variable should point to the encrypted file path inside the container.

!!! example "Encrypted Git access token"
    To use an encrypted Git access token, create a text file with the token and encrypt it with SOPS:
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

!!! tip "You can later edit the encrypted file in-place with"
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