# doco-cd - Docker Compose Continuous Deployment

## GitOps for Docker Compose

<img src="https://raw.githubusercontent.com/wiki/kimdre/doco-cd/images/doco-cd_logo.svg?t=20250714" alt="Doco CD Logo" height="48px" />

[![GitHub Release](https://img.shields.io/github/v/release/kimdre/doco-cd?display_name=tag&label=Release&color=47c72a&labelColor=404951)](https://github.com/kimdre/doco-cd/releases)
[![Go Report Card](https://goreportcard.com/badge/github.com/kimdre/doco-cd)](https://goreportcard.com/report/github.com/kimdre/doco-cd)
[![CodeQL](https://github.com/kimdre/doco-cd/actions/workflows/github-code-scanning/codeql/badge.svg)](https://github.com/kimdre/doco-cd/actions/workflows/github-code-scanning/codeql)
[![Tests](https://github.com/kimdre/doco-cd/actions/workflows/test.yaml/badge.svg)](https://github.com/kimdre/doco-cd/actions/workflows/test.yaml)
[![Build Image](https://github.com/kimdre/doco-cd/actions/workflows/build.yaml/badge.svg)](https://github.com/kimdre/doco-cd/actions/workflows/build.yaml)
[![Image Vulnerability Scan](https://github.com/kimdre/doco-cd/actions/workflows/image-vulnerability-scanning.yml/badge.svg?event=schedule)](https://github.com/kimdre/doco-cd/actions/workflows/image-vulnerability-scanning.yml)


Doco-CD is a lightweight GitOps tool that automatically deploys and updates Docker Compose projects/services and Swarm stacks using polling and webhooks.

You can think of it as a simple Portainer or ArgoCD alternative for Docker.

## Features

- Easy to set up and use.
- Runs with a minimal (distroless) image
- Built in Go with tiny RAM and CPU requirements.
- Supports various [external secret management providers](https://github.com/kimdre/doco-cd/wiki/External-Secrets) and data encryption with [SOPS](https://github.com/kimdre/doco-cd/wiki/Encryption)
- Can deploy applications via webhooks and/or polling.
- Supports various Git providers
- Supports both Docker Compose projects and Swarm stacks in [Swarm mode](https://github.com/kimdre/doco-cd/wiki/Swarm-Mode).
- Provides [notifications](https://github.com/kimdre/doco-cd/wiki/Notifications) and [Prometheus metrics](https://github.com/kimdre/doco-cd/wiki/Endpoints#prometheus-metrics) for monitoring.

Doco-CD supports both Docker Compose projects and Swarm stacks in [Swarm mode](https://github.com/kimdre/doco-cd/wiki/Swarm-Mode).

## Documentation

You can find the documentation in the [Wiki](https://github.com/kimdre/doco-cd/wiki).

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