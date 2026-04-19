# Documentation Guidelines

The `/wiki` directory contains the versioned documentation site for `doco-cd` built with [Zensical](https://github.com/zensical/zensical).

## Local development

Make sure you are in the root of the repository, then run the following commands:

1. Install the docs toolchain (requires Python 3.10+):

    ```bash
    make wiki-tools
    ```

2. Start the local docs server:

    ```bash
    make wiki-serve
    ```

3. Open the local URL printed by Zensical (usually http://localhost:8000).

    Edit Markdown files in `wiki/docs/` and refresh to see changes.

If you prefer running commands directly instead of Make targets:

```bash
python3 -m venv .venv-wiki
source .venv-wiki/bin/activate
pip install -r wiki/requirements.txt
zensical serve --config-file wiki/zensical.toml
```

## Cleaning the build cache

Sometimes you may need to clear Zensical's build cache to resolve issues with stale content.
This happens especially when trying to update embedded source code snippets, that are outside the wiki directory (e.g. `--8<-- "main.go"`).

You can do this with the following command:

```bash
rm -r wiki/.cache wiki/site
```

## Contributing to the docs

Please follow these rules when contributing to the wiki:

- Add or update user-facing pages in `wiki/docs/`, and avoid creating duplicate pages for topics that already exist.
- Keep content task-oriented: state the goal, provide ordered steps, and include expected results or troubleshooting notes when relevant.
- Use clear US English and keep terminology consistent with repository config names, environment variables, and CLI flags.
- Use a logical heading hierarchy (`#` -> `##` -> `###`) and descriptive heading text so sections are easy to scan.
- Prefer relative links for internal docs pages and verify links/anchors in the local preview.
- Keep command examples copy-pasteable and explicit; include required context such as file paths, prerequisites, and flags.
- Validate embedded snippets/references (for example `--8<--` includes) after changes, especially when referencing files outside `wiki/`.
- Preview every docs change locally with `make wiki-serve` and resolve rendering issues before submitting.
- For release-specific behavior, include the affected version and avoid hardcoding paths that assume `latest`.


## Publishing and versioning

The workflow in `.github/workflows/docs.yaml` publishes docs to the `gh-pages` branch when a GitHub release is published, which GitHub Pages serves from.

Supported release tag formats:

- stable: `vX.Y.Z`
- prerelease: `vX.Y.Z-rc.N` (release candidates)

Each release is published to `/<tag>/` (for example `/v1.2.3/`). The site root redirects to the latest stable release.

## Custom domain

`wiki/CNAME` is included so GitHub Pages serves the site at `doco.cd`.