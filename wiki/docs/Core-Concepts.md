---
tags:
  - Reference
---

# Core Concepts

This page explains the key terms and concepts used throughout the Doco CD documentation.
Familiarity with [Git](https://git-scm.com/), [Docker](https://docs.docker.com/), and basic [GitOps](https://about.gitlab.com/topics/gitops/) principles is assumed.

---

## How Doco CD Works

Doco CD follows a **GitOps** model: your Git repository is the single source of truth for both application code and deployment configuration.
When a change is pushed to Git, Doco CD detects it (via a webhook or poll), clones the repository, and applies the desired state to your Docker environment.

```mermaid
flowchart LR
    classDef trigger  fill:#4a90d9,stroke:#2c6fad,color:#fff
    classDef process  fill:#555,stroke:#333,color:#fff
    classDef decision fill:#e8a838,stroke:#b07a1a,color:#fff
    classDef output   fill:#3aaa6e,stroke:#1e7a4a,color:#fff
    classDef side     fill:#7b5ea7,stroke:#543d7a,color:#fff

    WH([Webhook]):::trigger      --> AUTH
    PL([Polling]):::trigger      --> AUTH
    AUTH["Authenticate\nToken / SSH"]:::process --> CLONE["Clone / Fetch\nRepository"]:::process
    CLONE --> CFG["Read\n.doco-cd.yml"]:::process
    CFG   --> SEC["Resolve Secrets\n& Decrypt SOPS"]:::process
    SEC   --> PRE["Pre-deploy\nScripts"]:::process
    PRE   --> SW{"Swarm\nMode?"}:::decision
    SW -->|Yes| SWARM["docker stack\ndeploy"]:::output
    SW -->|No|  COMP["docker compose\nup"]:::output
    SWARM --> POST["Post-deploy\nScripts"]:::process
    COMP  --> POST
    POST  --> DONE([Services\nRunning]):::output
    DONE  --> NOTIF["Notify\nvia Apprise"]:::side
    DONE  --> MTR["Prometheus\nMetrics"]:::side
```

---

## Deployment Targets

**Project**
:   A named collection of services defined in one or more `docker-compose.yml` files, deployed together in a standalone Docker environment.
    Each project has a unique name used to identify it both within Doco CD and in the Docker environment.

**Stack**
:   The equivalent of a project when [Docker Swarm mode](Advanced/Swarm-Mode.md) is enabled.
    Stacks are deployed and managed by Docker Swarm and support multi-node distribution and high availability.
    Doco CD automatically detects whether the Docker daemon is running in Swarm mode and deploys accordingly.

---

## Deployment Triggers

**Webhook**
:   An event-based HTTP notification sent by your Git provider (GitHub, GitLab, Gitea, etc.) to Doco CD whenever a commit is pushed.
    Webhooks are the recommended trigger method — they are fast and efficient, but require Doco CD to be reachable from the internet or local network.

    Enabled by setting the `WEBHOOK_SECRET` environment variable. See [Setup Webhook](Setup-Webhook.md) and [Webhook Listener](Endpoints/Webhook-Listener.md) for details.

**Polling**
:   A time-based trigger that checks one or more Git repositories for new commits at a configurable interval.
    Polling does not require Doco CD to be publicly reachable and is useful in isolated network environments, but is slower and less efficient than webhooks.

    Configured via the `POLL_CONFIG` or `POLL_CONFIG_FILE` environment variable. See [Poll Settings](Poll-Settings.md) for details.

!!! tip
    Both trigger methods can be used simultaneously. For example, you can use webhooks for immediate deployments and polling as a fallback for repositories that don't support webhooks.

---

## Deployment Process

**Deployment**
:   The process of applying a [deployment configuration](#deployment-configuration) to a project or stack.
    A deployment is triggered either by an incoming [webhook event](Endpoints/Webhook-Listener.md) or by [polling](Getting-Started.md#polling) a Git repository for changes.

    During a deployment, Doco CD will:

    1. Pull the latest changes from the Git repository.
    2. Resolve any [external secrets](External-Secrets/index.md) or [encrypted values](Advanced/Encryption.md).
    3. Build Docker images if necessary.
    4. Deploy the services defined in the `docker-compose.yml` files.

**Deployment Configuration**
:   A YAML file (`.doco-cd.yml` or `.doco-cd.yaml`) placed in the root of a Git repository that controls how a deployment is performed.
    It specifies the working directory, compose files, environment variables, and other deployment settings.

    See [Deploy Settings](Deploy-Settings.md) for the full list of available options.

**Poll Configuration**
:   A time-based trigger definition that instructs Doco CD to check one or more Git repositories for changes at a regular interval.
    Polling is an alternative to webhooks and does not require Doco CD to be reachable from the internet.

    See [Poll Settings](Poll-Settings.md) for configuration details.

**Deployment Loop**
:   A situation where a deployment continuously re-triggers itself, for example when a compose file is updated as part of the deployment process.
    Doco CD detects consecutive deployments for the same commit and, after a configurable threshold (`MAX_DEPLOYMENT_LOOP_COUNT`), forces a re-deployment to break the loop.
    Set `MAX_DEPLOYMENT_LOOP_COUNT` to `0` to disable detection entirely.

    See [App Settings](App-Settings.md) for configuration details.

---

## Secrets & Security

**External Secrets**
:   Secrets fetched at deployment runtime from an external secret manager (e.g. AWS Secrets Manager, Bitwarden, 1Password, Infisical).
    They are injected into the deployment environment as environment variables or Docker secrets, avoiding the need to store sensitive values in the Git repository or in Doco CD's own configuration.

    See [External Secrets](External-Secrets/index.md) for supported providers and usage.

**Encryption (SOPS)**
:   Doco CD supports decrypting files encrypted with [SOPS](https://getsops.io/) at deployment time.
    This allows sensitive values in deployment and compose files to be stored encrypted in the Git repository.

    See [Encryption](Advanced/Encryption.md) for setup instructions.
