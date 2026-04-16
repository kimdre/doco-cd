This page shows how to set up an access token for your deployments.

> The Git access token is used to authenticate with your Git provider (GitHub, GitLab, Bitbucket, etc.) and to clone or fetch your repositories via HTTP.
> 
> You can use doco-cd without a Git access token if the repositories you want to use for your deployments are publicly accessible. However, it is still recommended to use one in that case to for example avoid rate limits.
> 
> If you set a Git access token, doco-cd will always use it to authenticate with your Git provider.

## Git Providers



- [GitHub](#github)
- [Gitea, Forgejo, Gogs](#gitea-forgejo-and-gogs)
- [Gitlab](#gitlab)
- [Azure DevOps](#azure-devops)

## GitHub

You can either use a personal access token or a GitHub App.

### How to create an access token

See the GitHub docs for
- [Personal access tokens](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens).
- [GitHub Apps](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app).

### Permissions

**Personal access token (Classic)**

- The minimum required scope is `repo`

**Fine-grained tokens**

- Repository access
  - Set to `Public Repositories (read-only)` for only public repositories.
  - Set to `All Repositories` for all repositories.
- The minimum required permissions are:
  - `Contents` -> `Read-only`
  - `Metadata` -> `Read-only`

## Gitea, Forgejo and Gogs

- Go to your user settings.
- Click on `Applications`.
- Under `Generate New Token`: 
  - Fill in the `Token Name` field.
  - Set `Repository and Organization Access` to `All`
  - Open `Select Permissions` and set `repository` to `Read`
  - Click on `Generate Token` and save the token that is shown on the top of the page.

## Gitlab

You can either use a personal access token, a group access token or a project access token for Gitlab.

Recommended are personal or group access tokens, as they can be used for multiple projects/repositories.

### Token Permissions

- The role `Reporter` is sufficient (if asked).
- The minimum required scope is `read_repository`.

### How to create an access token

See the Gitlab docs for
- [Personal access tokens](https://docs.gitlab.com/ee/user/profile/personal_access_tokens.html).
- [Group access tokens](https://docs.gitlab.com/ee/user/group/settings/group_access_tokens.html).
- [Project access tokens](https://docs.gitlab.com/ee/user/project/settings/project_access_tokens.html).


## Azure DevOps

- Follow the [official Microsoft documentation](https://learn.microsoft.com/en-us/azure/devops/organizations/accounts/use-personal-access-tokens-to-authenticate?view=azure-devops&tabs=Windows) to create a Personal Access Token (PAT).
