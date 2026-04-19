# doco-cd - Docker Compose Continuous Deployment

## GitOps for Docker Compose

<img src="https://raw.githubusercontent.com/kimdre/doco-cd/main/wiki/docs/images/doco-cd_logo.svg" alt="Doco CD Logo" height="48px" />

[![GitHub Release](https://img.shields.io/github/v/release/kimdre/doco-cd?display_name=tag&label=Release&color=47c72a&labelColor=404951)](https://github.com/kimdre/doco-cd/releases)
[![Go Report Card](https://goreportcard.com/badge/github.com/kimdre/doco-cd)](https://goreportcard.com/report/github.com/kimdre/doco-cd)
[![CodeQL](https://github.com/kimdre/doco-cd/actions/workflows/github-code-scanning/codeql/badge.svg)](https://github.com/kimdre/doco-cd/actions/workflows/github-code-scanning/codeql)
[![Tests](https://github.com/kimdre/doco-cd/actions/workflows/test.yaml/badge.svg)](https://github.com/kimdre/doco-cd/actions/workflows/test.yaml)
[![Build Image](https://github.com/kimdre/doco-cd/actions/workflows/build.yaml/badge.svg)](https://github.com/kimdre/doco-cd/actions/workflows/build.yaml)
[![Image Vulnerability Scan](https://github.com/kimdre/doco-cd/actions/workflows/image-vulnerability-scanning.yml/badge.svg?event=schedule)](https://github.com/kimdre/doco-cd/actions/workflows/image-vulnerability-scanning.yml)


Doco CD is a lightweight, declarative GitOps continuous delivery tool that automatically deploys and updates Docker Compose projects/services and Swarm stacks using polling and webhooks.

You can think of it as a simple Portainer or ArgoCD alternative for Docker.

## Features

- Easy to set up and use.
- Runs with a minimal (distroless) image
- Built in Go with tiny RAM and CPU requirements.
- Supports various [external secret management providers](https://doco.cd/latest/External-Secrets/) and data encryption with [SOPS](https://doco.cd/latest/Encryption/)
- Can deploy applications via webhooks and/or polling.
- Supports various [Git providers](https://doco.cd/latest/#supported-git-providers)
- Supports both Docker Compose projects and Swarm stacks in [Swarm mode](https://doco.cd/latest/Swarm-Mode/).
- Provides [notifications](https://doco.cd/latest/Notifications/) and [Prometheus metrics](https://doco.cd/latest/Endpoints/#prometheus-metrics) for monitoring.

Doco-CD supports both Docker Compose projects and Swarm stacks in [Swarm mode](https://doco.cd/latest/Swarm-Mode/).

## Documentation

You can find the documentation at [doco.cd](https://doco.cd/latest/).

## Community

- Ask questions or discuss ideas on [GitHub Discussions](https://github.com/kimdre/doco-cd/discussions)
- Report bugs or suggest features by [opening an issue](https://github.com/kimdre/doco-cd/issues/new)

## Contributing

Contributions are welcome! Please see the [contributing guidelines](https://github.com/kimdre/doco-cd/blob/main/CONTRIBUTING.md) for more information.

## Star History

<a href="https://www.star-history.com/#kimdre/doco-cd&Date">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/svg?repos=kimdre/doco-cd&type=Date&theme=dark" />
   <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/svg?repos=kimdre/doco-cd&type=Date" />
   <img alt="Star History Chart" src="https://api.star-history.com/svg?repos=kimdre/doco-cd&type=Date" />
 </picture>
</a>