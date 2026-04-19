# wiki

This directory contains the new versioned documentation site for `doco-cd` built with [Zensical](https://github.com/zensical/zensical).

## Local development

Create and use a virtual environment, then run the local preview:

```bash
python3 -m venv .venv-wiki
source .venv-wiki/bin/activate
pip install -r wiki/requirements.txt
zensical serve --config-file wiki/zensical.toml
```

## Build

```bash
source .venv-wiki/bin/activate
zensical build --config-file wiki/zensical.toml --clean
```

The generated site is written to `wiki/site/`.

## Publishing and versioning

The workflow in `.github/workflows/docs-release.yaml` publishes docs to the `gh-pages` branch when a GitHub release is published.

Supported release tag formats:

- stable: `vX.Y.Z`
- prerelease: `vX.Y.Z-rc.1` (and other semver prerelease forms)

Each release is published to `/<tag>/` (for example `/v1.2.3/`). The site root redirects to the latest stable release.

## Custom domain

`wiki/CNAME` is included so GitHub Pages serves the site at `doco.cd`.

