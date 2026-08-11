---
tags:
  - Setup
  - Configuration
  - Automation
---

# Renovate

[Renovate](https://docs.renovatebot.com/) is the perfect companion for your [GitOps](../Core-Concepts.md#how-doco-cd-works) workflow.
It keeps dependencies, Docker images, GitHub Actions, and other package references up to date by opening pull requests or merge requests automatically in your Git repositories.

This guide shows:

1. How to add a recommended `renovate.json`
2. How to enable Renovate on GitHub and GitLab
3. How to run Renovate against self-hosted Forgejo and GitLab instances

!!! tip "Dependabot"
    If you are using GitHub, you may also consider [Dependabot](https://docs.github.com/en/code-security/tutorials/secure-your-dependencies/dependabot-quickstart) as an alternative to Renovate.
    It is maintained by GitHub and has a simpler configuration, but it is less flexible and does not support all package types.

    I still recommend Renovate however because it is more flexible and supports more package types.

## Example `renovate.json`

Add a `renovate.json` file to the root of your repository:

```json title="renovate.json"
{
  "$schema": "https://docs.renovatebot.com/renovate-schema.json",
  "extends": [
    "config:best-practices",
    ":timezone(Europe/Berlin)",
    ":dependencyDashboard",
    ":separateMajorReleases"
  ],
  "labels": ["dependencies"],
  "automergeSchedule": ["* 3-7 * * *"],
  "platformAutomerge": false,
  "packageRules": [
    {
      "matchUpdateTypes": ["patch", "pin"],
      "automerge": true
    },
    {
      "matchUpdateTypes": ["minor"],
      "automerge": false
    },
    {
      "matchUpdateTypes": ["major"],
      "automerge": false
    }
  ]
}
```

This example configuration:

- Uses Renovate's best practices
- Opens a dependency dashboard issue for pending updates
- Labels all update PRs/MRs with `dependencies`
- Separates major updates into their own PRs/MRs
- Automerges patch updates, but not minor or major updates
- Runs automerge only during quiet hours (3am-7am in Berlin time)

!!! tip "Renovate documentation"
    You can always find more options in the [Renovate configuration reference](https://docs.renovatebot.com/configuration-options/).

## Setup Renovate Bot

=== "GitHub"

    For repositories on `github.com`, the easiest option is the hosted **Mend Renovate App**.

    **Setup**

    1. Open the [Renovate GitHub App](https://github.com/apps/renovate).
    2. Install it for your user or organization.
    3. Choose either **All repositories** or **Only select repositories**.
    4. Make sure your repository contains the `renovate.json` file shown above, or wait for the onboarding PR and edit it there.
      1. Merge the onboarding PR.

    **Day-to-day usage**

    - Renovate opens PRs for dependency updates.
    - Major updates are usually separate and easier to review.
    - Non-major updates are often grouped by Renovate's presets.
    - Use labels, reviewers, and branch protections the same way you would for normal PRs.
    - To debug Renovate runs, check the logs at [Mend.io](https://developer.mend.io/) or in the PR comments/Renovate dashboard issue.

    !!! note
        If you use GitHub Enterprise Server or want full control over the bot, run Renovate as a self-hosted service instead of the hosted app.

=== "GitLab"

    === "GitLab.com"

        The hosted Renovate app for [gitlab.com](https://gitlab.com/) is currently unavailable, so the recommended approach is to run Renovate with GitLab CI using the official `renovate-runner` project or your own scheduled pipeline.
    
        **Setup**
    
        1. Create a dedicated bot user or access token for Renovate.
        2. Give the bot at least **Developer** access to the project.
        3. If you want automerge on protected branches, also give it the permissions required to merge there.
        4. Add your `renovate.json` file to the repository.
        5. Create a scheduled pipeline that runs Renovate regularly, ideally hourly.
    
        **Minimal GitLab CI example**
    
        ```yaml title=".gitlab-ci.yml"
        include:
          - project: "renovate-bot/renovate-runner"
            file: "/templates/renovate.gitlab-ci.yml"
        ```
    
        Set these CI/CD variables in the project or group settings:
    
        - `RENOVATE_TOKEN`: GitLab access token for the Renovate bot
        - `RENOVATE_PLATFORM`: `gitlab`
    
        For `gitlab.com`, no custom endpoint is needed.
    
        **Day-to-day usage**
    
        - Renovate opens merge requests on its schedule.
        - Merge the onboarding MR first.
        - After that, review and merge update MRs as usual.

    === "Self-hosted GitLab"
    
        For self-hosted instances, the recommended approach is to run Renovate in CI on a schedule; alternatively, run it yourself with Docker or the CLI and point it at your platform API.
    
        **Setup**
    
        1. Create a dedicated bot account.
        2. Create a token for that bot account.
        3. Add the recommended `renovate.json` to each repository you want Renovate to manage.
        4. Run Renovate on a schedule.
        5. Store tokens in environment variables or secrets, not in the repository.
    
        Use:
    
        - `platform=gitlab`
        - `endpoint=https://gitlab.example.com/api/v4/`
    
        The Renovate bot or token should have at least **Developer** access to the repositories it manages.
        If your protected branch rules only allow maintainers to merge, the bot also needs the corresponding merge permissions.
    
        **Example self-hosted config**
    
        This is **not** committed to the target repository. It is the global config for the Renovate service you run yourself.
    
        ```js title="config.js"
        module.exports = {
          platform: 'gitlab',
          endpoint: 'https://gitlab.example.com/api/v4/',
          token: process.env.RENOVATE_TOKEN,
          autodiscover: true,
          onboardingConfig: {
            extends: ['config:best-practices']
          }
        };
        ```
    
        **Minimal self-hosted GitLab pipeline example**
    
        ```yaml title=".gitlab-ci.yml"
        stages:
          - renovate
    
        renovate:
          stage: renovate
          image: renovate/renovate:latest
          script:
            - renovate
          variables:
            RENOVATE_PLATFORM: gitlab
            RENOVATE_ENDPOINT: https://gitlab.example.com/api/v4/
          rules:
            - if: '$CI_PIPELINE_SOURCE == "schedule"'
            - if: '$CI_PIPELINE_SOURCE == "web"'
        ```
    
        Set `RENOVATE_TOKEN` as a masked/protected CI/CD variable under **Settings > CI/CD > Variables** in your project or group. Do not add it to the YAML file. Renovate reads it automatically from the environment.
    
        To trigger the job, create a [scheduled pipeline](https://docs.gitlab.com/ci/pipelines/schedules/) (e.g. hourly), since the `rules` above only run on schedule or manual (`web`) triggers.
    
        **Day-to-day usage**
    
        - Renovate opens merge requests on its schedule.
        - Merge the onboarding MR first.
        - After that, review and merge update MRs as usual.

=== "Forgejo"

    For self-hosted instances, the recommended approach is to run Renovate in CI on a schedule; 
    alternatively, run it yourself with Docker or the CLI and point it at your platform API.

    **Setup**

    1. Create a dedicated bot account.
    2. Create a token for that bot account.
    3. Add the recommended `renovate.json` to each repository you want Renovate to manage.
    4. Run Renovate on a schedule.
    5. Store tokens in environment variables or secrets, not in the repository.

    Use:

    - `platform=forgejo`
    - `endpoint=https://forgejo.example.com/api/v1/`

    The Personal Access Token should have these permissions:

    - `repo`: read and write
    - `user`: read
    - `issue`: read and write
    - `organization`: read

    If you use Forgejo Packages, also add `read:packages`.

    **Example self-hosted config**

    This is **not** committed to the target repository. It is the global config for the Renovate service you run yourself.

    ```js title="config.js"
    module.exports = {
      platform: 'forgejo',
      endpoint: 'https://forgejo.example.com/api/v1/',
      token: process.env.RENOVATE_TOKEN,
      autodiscover: true,
      onboardingConfig: {
        extends: ['config:best-practices']
      }
    };
    ```

    **Minimal Forgejo pipeline example**

    ```yaml title=".forgejo/workflows/renovate.yml"
    name: Renovate

    on:
      schedule:
        - cron: "0 * * * *"
      workflow_dispatch:

    jobs:
      renovate:
        runs-on: ubuntu-latest
        steps:
          - name: Run Renovate
            uses: docker://renovate/renovate:latest
            env:
              RENOVATE_TOKEN: ${{ secrets.RENOVATE_TOKEN }}
              RENOVATE_PLATFORM: forgejo
              RENOVATE_ENDPOINT: https://forgejo.example.com/api/v1/
    ```

    Store the bot token as a repository or organization secret named `RENOVATE_TOKEN` under **Settings > Actions > Secrets**. Never commit it to the workflow file.

    **Day-to-day usage**

    - Renovate opens pull requests on its schedule.
    - Merge the onboarding PR first.
    - After that, review and merge update PRs as usual.

!!! tip
    Run self-hosted Renovate at least hourly so onboarding PRs, rebases, and security-related updates are not delayed unnecessarily.

!!! info
    Renovate is a third-party tool and not part of Doco CD itself. It is maintained by [Mend.io](https://www.mend.io/).
