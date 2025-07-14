# doco-cd - Docker Compose Continuous Deployment

## GitOps for Docker Compose

<img src="https://raw.githubusercontent.com/wiki/kimdre/doco-cd/images/doco-cd_logo.svg?t=20250714" alt="Doco CD Logo" height="48px" />

[![GitHub Release](https://img.shields.io/github/v/release/kimdre/doco-cd?display_name=tag&label=Release)](https://github.com/kimdre/doco-cd/releases)
[![Go Report Card](https://goreportcard.com/badge/github.com/kimdre/doco-cd)](https://goreportcard.com/report/github.com/kimdre/doco-cd)
[![CodeQL](https://github.com/kimdre/doco-cd/actions/workflows/github-code-scanning/codeql/badge.svg)](https://github.com/kimdre/doco-cd/actions/workflows/github-code-scanning/codeql)
[![Tests](https://github.com/kimdre/doco-cd/actions/workflows/test.yaml/badge.svg)](https://github.com/kimdre/doco-cd/actions/workflows/test.yaml)
[![Spell checking](https://github.com/kimdre/doco-cd/actions/workflows/spelling.yaml/badge.svg)](https://github.com/kimdre/doco-cd/actions/workflows/spelling.yaml)
[![codecov](https://codecov.io/gh/kimdre/doco-cd/graph/badge.svg?token=TR4H8ATPL0)](https://codecov.io/gh/kimdre/doco-cd)
[![Build Image](https://github.com/kimdre/doco-cd/actions/workflows/build.yaml/badge.svg)](https://github.com/kimdre/doco-cd/actions/workflows/build.yaml)
[![Container Image Vulnerability Scanning](https://github.com/kimdre/doco-cd/actions/workflows/image-vulnerability-scanning.yml/badge.svg?event=schedule)](https://github.com/kimdre/doco-cd/actions/workflows/image-vulnerability-scanning.yml)


Doco CD is a lightweight GitOps tool that automatically deploys and updates Docker Compose projects/services using polling and webhooks.

You can think of it as a simple Portainer or ArgoCD alternative for Docker.

## Features

- **Simple setup**: Doco CD is easy to set up and use.
- **Secure**: The application runs with a minimal (distroless) image and supports webhook authentication and data encryption with [SOPS](https://getsops.io/).
- **Customizable**: The application can be configured using [environment variables](https://github.com/kimdre/doco-cd/wiki/App-Settings).
- **Flexible**: The deployments can be configured using a [deployment configuration](https://github.com/kimdre/doco-cd/wiki/Deploy-Settings) file.
- **Lightweight**: The application is built in Go and has tiny memory and CPU requirements.

## Documentation

You can find the documentation in the [Wiki](https://github.com/kimdre/doco-cd/wiki).

## Community

- Ask questions on [GitHub Discussions](https://github.com/kimdre/doco-cd/discussions)
- Report bugs or suggest features by [opening an issue](https://github.com/kimdre/doco-cd/issues/new)
- Contribute by [opening a pull request](https://github.com/kimdre/doco-cd/pulls)
