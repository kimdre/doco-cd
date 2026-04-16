---
title: ""
---

# Home

<img src="images/doco-cd_logo.svg" alt="Doco CD Logo" height="48px" />

## What is Doco CD?

**Doco-CD** stands for _**Do**cker **Co**mpose **C**ontinuous **D**eployment_ and is a lightweight GitOps tool 
that automatically deploys and updates Docker Compose projects and Swarm stacks via webhooks or polling when a change is pushed to a Git repository.

You can think of it as a simple Portainer or ArgoCD alternative for Docker.

## Features

- Easy to set up and use.
- Runs with a minimal (distroless) image
- Built in Go with tiny RAM and CPU requirements.
- Supports various [external secret management providers](external-secrets/index.md) and data encryption with [SOPS](Encryption.md)
- Can deploy applications via [webhooks](Quickstart.md#webhooks) and/or [polling](Quickstart.md#polling).
- Supports various [Git providers](#supported-git-providers) 
- Supports both Docker Compose projects and Swarm stacks in [Swarm mode](Swarm-Mode.md).
- Provides [notifications](Notifications.md) and [Prometheus metrics](Endpoints/Metrics.md) for monitoring.

## Getting Started

Follow the [Quickstart Guide](Quickstart.md) to get started with Doco CD. 

More resources:

1. [Tips and Tricks](Tips-and-Tricks.md) - Some tips and tricks for using the application.
2. [Known Limitations](Known-Limitations.md) - Learn about the limitations of the application.

See the right sidebar for links to the wiki pages with more detailed information.


## Supported Git Providers

See more info here: [Setup Webhook](Setup-Access-Token.md#git-providers)

- GitHub
- GitLab
- Gitea
- Forgejo
- Gogs
- Azure DevOps* ([_Service Hooks_ not supported](Setup-Webhook.md#azure-devops))

## Releases and Changelog

[![GitHub Release](https://img.shields.io/github/v/release/kimdre/doco-cd?include_prereleases&sort=semver&display_name=release&style=flat-square&label=Latest%20Version&color=%234CBB17)](https://github.com/kimdre/doco-cd/releases)
![GitHub Release Date](https://img.shields.io/github/release-date/kimdre/doco-cd?style=flat-square&label=Release%20Date&color=%234CBB17)

See the [releases page](https://github.com/kimdre/doco-cd/releases) for release notes and changelogs.

## Image

You can find the Docker image in the [GitHub Container Registry](https://github.com/kimdre/doco-cd/pkgs/container/doco-cd).

```sh
docker pull ghcr.io/kimdre/doco-cd:latest
```

To use a specific version, replace `latest` with the desired release version without the leading `v` (e.g. `0.1.0`):

```sh
ghcr.io/kimdre/doco-cd:0.1.0
```

## Community

- Ask questions on [GitHub Discussions](https://github.com/kimdre/doco-cd/discussions)
- Report bugs or suggest features by [opening an issue](https://github.com/kimdre/doco-cd/issues/new)

## Contributing

Contributions are welcome! Please see the [contributing guidelines](https://github.com/kimdre/doco-cd/blob/main/CONTRIBUTING.md) for more information.
