---
title: "Doco-CD Documentation"
---

# Doco-CD Documentation

![Doco-CD Logo](images/doco-cd_logo.svg)

## What is Doco-CD?

**Doco-CD** stands for _**Do**cker **Co**mpose **C**ontinuous **D**eployment_ and is a lightweight GitOps tool 
that automatically deploys and updates Docker Compose projects and Swarm stacks via webhooks or polling when a change is pushed to a Git repository.

You can think of it as a simple Portainer or ArgoCD alternative for Docker.

## Features

- Easy to set up and use.
- Runs with a minimal (distroless) image
- Built in Go with tiny RAM and CPU requirements.
- Supports both Docker Compose projects and Swarm stacks in [Swarm mode](Advanced/Swarm-Mode.md).
- Can deploy applications via [webhooks](Getting-Started.md#webhooks) and/or [polling](Getting-Started.md#polling).
- Supports various [Git providers](#supported-git-providers)
- Supports [OCI artifacts](Advanced/OCI/Artifact-Usage.md) and Git repositories for deployment configurations.
- Supports various [external secret management providers](External-Secrets/index.md) and data encryption with [SOPS](Advanced/Encryption.md)
- Supports [Job Scheduling / Cron Jobs](Advanced/Job-Scheduling.md) for running periodic tasks.
- Provides [Notifications](Advanced/Notifications.md) and [Prometheus metrics](Endpoints/Metrics.md) for monitoring.

## Getting Started

Follow the [Getting Started Guide](Getting-Started.md) to get started with Doco-CD. 

More resources:

1. [Core Concepts](Core-Concepts.md) - Learn about the core concepts of Doco-CD.
2. [Known Limitations](Known-Limitations.md) - Learn about the limitations of the application.
3. [Tips and Tricks](Advanced/Tips-and-Tricks.md) - Some tips and tricks for using the application.
4. [Self-Updating](Advanced/Self-Updating.md) - Run two Doco-CD instances that manage each other for self-updating.

## Supported Git Providers

See more info here: [Setup Webhook](Setup-Access-Token.md#git-providers)

- GitHub
- GitLab
- Gitea
- Forgejo
- Gogs
- Azure DevOps* ([_Service Hooks_ not supported](Setup-Webhook.md#setup-in-git-providers-azure-devops))

## Releases and Changelog

[![GitHub Release](https://img.shields.io/github/v/release/kimdre/doco-cd?include_prereleases&sort=semver&display_name=release&style=flat-square&label=Latest%20Version&color=%234CBB17)](https://github.com/kimdre/doco-cd/releases)
![GitHub Release Date](https://img.shields.io/github/release-date/kimdre/doco-cd?style=flat-square&label=Release%20Date&color=%234CBB17)

See the [releases page](https://github.com/kimdre/doco-cd/releases) for release notes and changelogs or the [Security Policy](Security.md) for more information.

## Image

You can find the Docker image in the [GitHub Container Registry](https://github.com/kimdre/doco-cd/pkgs/container/doco-cd).

```sh
docker pull ghcr.io/kimdre/doco-cd:latest
```

To use a specific version, replace `latest` with the desired release version without the leading `v` (e.g. `0.103.0`):

```sh
ghcr.io/kimdre/doco-cd:0.103.0
```

## Community

- Ask questions on [GitHub Discussions](https://github.com/kimdre/doco-cd/discussions)
- Report bugs or suggest features by [opening an issue](https://github.com/kimdre/doco-cd/issues/new/choose)

## Contributing

Contributions are welcome! Please see the [Contributing Guidelines](Contributing.md) for more information.

## In the Media

Doco-CD has been featured by industry media and technical publications:

| Date       | Publication | Article                                                                                                                    |
|------------|-------------|----------------------------------------------------------------------------------------------------------------------------|
| 2026-05-01 | c't Magazin | [(German) c't 10/2026](https://www.heise.de/select/ct/2026/10/2609115553794560316)                                         |
| 2026-04-22 | heise+      | [(German) Watchtower and alternatives: how to keep Docker containers automatically up to date](https://heise.de/-11243856) |
| 2025-11-14 | selfh.st    | [Weekly: 2025-11-14](https://selfh.st/weekly/2025-11-14/)                                                                  |

## Support the Project

If you would like to support the development of Doco CD, please consider becoming a sponsor on [GitHub Sponsors](https://github.com/sponsors/kimdre).

Doco-CD is proudly supported by

<a href="https://jb.gg/OpenSource">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://www.jetbrains.com/company/brand/img/logo_jb_dos_3.svg" />
   <source media="(prefers-color-scheme: light)" srcset="https://resources.jetbrains.com/storage/products/company/brand/logos/jetbrains.svg" />
   <img alt="JetBrains logo" src="https://resources.jetbrains.com/storage/products/company/brand/logos/jetbrains.svg" />
 </picture>
</a>

[![sponsors badge](https://readme-contribs.as93.net/sponsors/kimdre?shape=circle&fontSize=14)](https://github.com/sponsors/kimdre)

## Star History

<a href="https://www.star-history.com/?repos=kimdre%2Fdoco-cd&type=date&legend=top-left">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/chart?repos=kimdre/doco-cd&type=date&theme=dark&legend=top-left&sealed_token=saZ0hV4u45f86P2hpUFWpOZm-TTGM2b4M-z2Ji3FRAzBKzsZ5l1JmPPstp4FYVSkLI4mqT3OqCnGxuiJEhZ1KDWZs7P39Slsvd_dIJfC1tCRRsi5HafLag" />
   <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/chart?repos=kimdre/doco-cd&type=date&legend=top-left&sealed_token=saZ0hV4u45f86P2hpUFWpOZm-TTGM2b4M-z2Ji3FRAzBKzsZ5l1JmPPstp4FYVSkLI4mqT3OqCnGxuiJEhZ1KDWZs7P39Slsvd_dIJfC1tCRRsi5HafLag" />
   <img alt="Star History Chart" src="https://api.star-history.com/chart?repos=kimdre/doco-cd&type=date&legend=top-left&sealed_token=saZ0hV4u45f86P2hpUFWpOZm-TTGM2b4M-z2Ji3FRAzBKzsZ5l1JmPPstp4FYVSkLI4mqT3OqCnGxuiJEhZ1KDWZs7P39Slsvd_dIJfC1tCRRsi5HafLag" />
 </picture>
</a>