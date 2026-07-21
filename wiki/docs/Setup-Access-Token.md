---
tags:
  - Setup
  - Configuration
---

# Setup Git Access Token

This page shows how to set up a Git Access Token for your deployments.

The Git Access Token is used to authenticate with your Git provider (GitHub, GitLab, Bitbucket, etc.) and to clone or fetch your repositories via HTTP.

!!! tip "Usage without Git Access Token"
    You can use doco-cd without a Git Access Token if the repositories you want to use for your deployments are publicly accessible. However, it is still recommended to use one in that case to for example avoid rate limits.

!!! info "About Git Authentication"
    See [Git Authentication](Git-Settings.md#authentication) for more information on how doco-cd handles Git authentication
    and how to set up global and per-domain credentials.

!!! info "Using GitHub Apps"
    If you use GitHub, you can also authenticate using a [GitHub App](Git-Settings.md#github-apps).

## Git Providers

--8<-- "wiki/includes/git-access-token-permissions.md"
