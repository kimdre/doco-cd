This page shows how to set up SSH keys for your deployments.

!!! quote "About SSH Keys"
    SSH keys are used to authenticate with your Git provider (GitHub, GitLab, Bitbucket, etc.) and to clone or fetch your repositories via SSH.
    You need to set up SSH keys if you use SSH URLs for your Git repositories (e.g. `git@github.com:kimdre/doco-cd.git`).

## Generate SSH Key Pair

You can generate a new SSH key pair using the `ssh-keygen` command. Run the following command in your terminal:

```sh
ssh-keygen -t ed25519 -C "doco-cd-deployment-key"
```

This will create a new SSH key pair using the Ed25519 algorithm. 
You will be prompted to enter a file path to save the key pair and a passphrase (optional). 
If you leave the file path empty, the keys will be saved in the default location (`~/.ssh/id_ed25519` and `~/.ssh/id_ed25519.pub`).

## Add Public Key to Git Provider
After generating the SSH key pair, add the public key (`id_ed25519.pub` or the file path you specified) 
to your Git provider.
You can either add the public key as a Deploy Key for a specific repository/organization or as an SSH key for your user account.

### Test SSH Connection
You can test the SSH connection to your Git provider using the following command([GitHub Docs](https://docs.github.com/en/authentication/connecting-to-github-with-ssh/testing-your-ssh-connection)):
```sh
ssh -T git@<your-git-provider>
```

!!! note
    You may need to define the SSH private key to use with the `-i` option if it's not the default key.
    For example:
    ```sh
    ssh -i /path/to/your/id_ed25519 -T git@<your-git-provider>
    ```

Replace `<your-git-provider>` with the appropriate domain for your Git provider (e.g. `github.com`, `gitlab.com`, etc.).

If the connection is successful, you should see a message indicating that you have successfully authenticated.

## Configure doco-cd to use the Private Key
You need to configure doco-cd to use the private key (`id_ed25519` or the file path you specified) for SSH authentication.
See the app config on the [App Settings](App-Settings.md) wiki page for more information on how to set the SSH private key in doco-cd.

An example using Docker Compose:

```yaml title="docker-compose.yml"
services:
  app:
    container_name: doco-cd
    environment:
      SSH_PRIVATE_KEY_FILE: /run/secrets/ssh_private_key
      SSH_PRIVATE_KEY_PASSPHRASE: ""  # Optional, only if you set a passphrase when generating the key
    secrets:
      - ssh_private_key
      
secrets:
  ssh_private_key:
    file: ./path/to/your/id_ed25519
```
