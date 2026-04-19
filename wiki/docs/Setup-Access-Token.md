---
tags:
  - Setup
  - Configuration
---

# Setup Git Access Token

This page shows how to set up a Git Access Token for your deployments.

!!! info
    The Git Access Token is used to authenticate with your Git provider (GitHub, GitLab, Bitbucket, etc.) and to clone or fetch your repositories via HTTP.
    
    !!! tip
        You can use doco-cd without a Git Access Token if the repositories you want to use for your deployments are publicly accessible. However, it is still recommended to use one in that case to for example avoid rate limits.
    
    If you set a Git Access Token, doco-cd will always use it to authenticate with your Git provider.

## Git Providers

=== "GitHub"
    You can either use a Personal Access Token (PAT) or a GitHub App.
    
    !!! question "How to create an access token"
        See the GitHub docs for

        - [Personal Access Tokens](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens).
        - [GitHub Apps](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app).
    
    **Permissions**
    
    === "Personal Access Token (Classic)"
    
        - The minimum required scope is `repo`
    
    === "Fine-grained tokens"
    
        - Repository access
            - Set to `Public Repositories (read-only)` for only public repositories.
            - Set to `All Repositories` for all repositories.
        - The minimum required permissions are:
            - `Contents` -> `Read-only`
            - `Metadata` -> `Read-only`

=== "Gitea, Forgejo, Gogs" 
    1. Go to your user settings.
    2. Click on `Applications`.
    3. Under `Generate New Token`: 
        1. Fill in the `Token Name` field.
        2. Set `Repository and Organization Access` to `All`
        3. Open `Select Permissions` and set `repository` to `Read`
        4. Click on `Generate Token` and save the token that is shown on the top of the page.

=== "Gitlab"

    You can either use a personal access token, a group access token or a project access token for Gitlab.
    
    !!! tip "Which token to use?"
        Recommended are personal or group access tokens, as they can be used for multiple projects/repositories.
    
    !!! question "How to create an access token?"
    
        See the Gitlab docs for
    
        - [Personal access tokens](https://docs.gitlab.com/ee/user/profile/personal_access_tokens.html).
        - [Group access tokens](https://docs.gitlab.com/ee/user/group/settings/group_access_tokens.html).
        - [Project access tokens](https://docs.gitlab.com/ee/user/project/settings/project_access_tokens.html).

    **Token Permissions**
    
    - The role `Reporter` is sufficient (if asked).
    - The minimum required scope is `read_repository`.


=== "Azure DevOps"

    - Follow the [official Microsoft documentation](https://learn.microsoft.com/en-us/azure/devops/organizations/accounts/use-personal-access-tokens-to-authenticate?view=azure-devops&tabs=Windows) to create a Personal Access Token (PAT).
