---
tags:
  - OCI
  - Advanced
  - Configuration
---

# OCI Artifact Usage

This page provides comprehensive documentation on using doco-cd with OCI (Open Container Initiative) artifacts, including webhook payloads, and artifact packaging conventions.

## Overview

Doco-CD supports pulling deployment configurations from OCI registries (e.g., Docker Hub, GitHub Container Registry, private registries) in addition to Git repositories. This allows you to:

- Store deployment configuration as versioned OCI artifacts
- Use container registries as your source of truth for deployments
- Trigger deployments via OCI webhook events
- Validate artifact signatures before deployment
- Use the same registry infrastructure for both container images and configuration

## Getting Started

To use OCI artifacts with doco-cd, you need to:

1. Package your deployment configuration according to the [`doco.v1` layout specification](#docov1-artifact-layout)
2. Push the artifact to an OCI registry
3. Configure doco-cd with either [polling](Polling.md) or [webhooks](Webhooks.md) to pull the artifact and trigger deployments
4. (Optional) Configure signature verification with [trust policies](Trust-Policy.md)

## Supported OCI Registries

Doco-cd can pull artifacts from any OCI-compliant registry that supports OCI Image Spec v1.0 or later. 
This includes, but is not limited to:

- **Docker Hub** (`docker.io`)
- **GitHub Container Registry** (`ghcr.io`)
- **GitLab Container Registry** (`registry.gitlab.com`)
- **Amazon ECR** (`*.dkr.ecr.*.amazonaws.com`)
- **Google Artifact Registry** (`*.pkg.dev`)
- **Azure Container Registry** (`*.azurecr.io`)
- **Private/Self-hosted registries** (supporting OCI Image Spec v1.0+)

!!! note "See [Private Container Registries](../Private-Container-Registries.md) for authentication to private registries."

---

## `doco.v1` Artifact Layout

The **doco.v1** layout is a strict, versioned specification for packaging deployment configurations as OCI artifacts. It ensures consistency and enables validation.

### Artifact Structure

A doco.v1 artifact must have a root-level deployment configuration file (`.doco-cd.y(a)ml`) in the root (`/`) of the artifact
that includes the `#!yaml layout: doco.v1` field. 

The rest of the artifact can contain any files needed for deployment, as with deployments from Git repository (e.g., compose files, app configuration, assets, scripts), see [Deploy Settings](../../Deploy-Settings.md).

!!! example "Artifact Layout Examples"

    === "Single Deployment"
        ```
        artifact-root/
        ├── .doco-cd.yaml        # Main deployment config with `layout: doco.v1`
        ├── docker-compose.yml    # Docker Compose configuration
        └── (other files as needed)
        ```
    
    === "Multiple Deployments"
        ```
        artifact-root/
        ├── .doco-cd.yaml        # Main deployment config with `layout: doco.v1`
        ├── web/
        │   ├── .doco-cd.yaml    # Extra deployment config for web service
        │   └── docker-compose.yml
        │   └── config/
        │       └── nginx.conf
        └── app/
            └── docker-compose.yml
            └── app.env
            └── migrations/
                └── 001-init.sql
        ```

### Required Files

- `.doco-cd.y(a)ml`: **Required**

    The main [deployment configuration](../../Deploy-Settings.md) file.
    
    **Layout version**: Each deployment configuration must include `#!yaml layout: doco.v1` to indicate it follows the doco.v1 artifact layout specification.
    ```yaml title=".doco-cd.yaml Example"
    layout: doco.v1
    name: production
    compose_files:
      - docker-compose.yml
    profiles:
      - production
    ```

### Example: Creating an OCI Artifact

Here's a complete example of creating and pushing a `doco.v1` OCI artifact:

1. Create the artifact directory
    ```sh
    mkdir -p artifact
    cd artifact
    
    # Create deployment configuration
    cat > .doco-cd.yaml << 'EOF'
    layout: doco.v1
    name: web-app
    compose_files:
      - docker-compose.yml
    EOF
    
    # Create docker-compose.yml
    cat > docker-compose.yml << 'EOF'
    services:
      app:
        image: myapp:latest
      nginx:
        image: nginx:latest
    EOF
    ```

2. Create the OCI artifact

    === "Using Docker CLI"
        ```sh
        # Create a minimal Dockerfile inside the artifact directory
        cat > artifact/Dockerfile << 'EOF'
        FROM scratch
        COPY . /
        EOF
        
        # Build and push the artifact
        docker build -t ghcr.io/myorg/myapp-config:main artifact/
        docker push ghcr.io/myorg/myapp-config:main
        ```
  
    === "Using OCI tools"
        We use [skopeo](https://github.com/containers/skopeo) to copy the directory directly to an OCI registry without needing to create a Docker image:
  
        ```sh
        # Using skopeo or similar tools
        skopeo copy dir://artifact oci:oras-artifact:latest
        ```
