"""Generate the deployment editor schema while building the documentation."""

from __future__ import annotations

import json
import subprocess
from functools import lru_cache
from hashlib import sha256
from pathlib import Path
from typing import Any

from markdown import Extension, Markdown
from zensical.config import get_config

REPOSITORY_ROOT = Path(__file__).resolve().parents[2]
SCHEMA_PATH = REPOSITORY_ROOT / "wiki/docs/schemas/deploy.schema.json"
GENERATOR_DIRECTORIES = (
    "cmd/deploy-schema",
    "internal/config",
    "internal/common/defaults",
    "internal/secretprovider/types",
)


def _source_fingerprint() -> str:
    digest = sha256()
    inputs = [REPOSITORY_ROOT / name for name in ("go.mod", "go.sum")]
    for directory in GENERATOR_DIRECTORIES:
        inputs.extend(
            path
            for path in (REPOSITORY_ROOT / directory).rglob("*.go")
            if not path.name.endswith("_test.go")
        )
    for path in sorted(inputs):
        digest.update(path.relative_to(REPOSITORY_ROOT).as_posix().encode())
        digest.update(b"\0")
        digest.update(path.read_bytes())
    return digest.hexdigest()


@lru_cache(maxsize=1)
def _generate_schema(_fingerprint: str) -> bytes:
    """Regenerate only when the generator or its configuration inputs change."""
    try:
        result = subprocess.run(
            ["go", "run", "./cmd/deploy-schema", "-output", "-"],
            cwd=REPOSITORY_ROOT,
            check=True,
            capture_output=True,
            timeout=120,
        )
    except FileNotFoundError as error:
        raise RuntimeError("The deployment schema requires Go to be installed and available on PATH") from error
    except subprocess.CalledProcessError as error:
        details = (error.stderr or error.stdout).decode(errors="replace").strip() or "no error output"
        raise RuntimeError(f"Could not generate the deployment schema: {details}") from error
    except subprocess.TimeoutExpired as error:
        raise RuntimeError("Generating the deployment schema exceeded the 120-second timeout") from error

    try:
        schema = json.loads(result.stdout)
    except (json.JSONDecodeError, UnicodeDecodeError) as error:
        raise RuntimeError("The deployment schema generator returned invalid JSON") from error
    if not isinstance(schema, dict) or "$schema" not in schema:
        raise RuntimeError("The deployment schema generator returned no JSON Schema")
    return result.stdout


class DeploySchemaExtension(Extension):
    """Keep the published schema in sync when Markdown extensions are loaded."""

    def extendMarkdown(self, md: Markdown) -> None:
        generated = _generate_schema(_source_fingerprint())
        _write_schema(SCHEMA_PATH, generated)
        # Zensical copies static assets before rendering Markdown.
        config = get_config()
        site_dir = Path(config["root_dir"]) / config["site_dir"]
        _write_schema(site_dir / "schemas/deploy.schema.json", generated)
        md.registerExtension(self)


def _write_schema(path: Path, generated: bytes) -> None:
    if not path.is_file() or path.read_bytes() != generated:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(generated)


def makeExtension(**kwargs: Any) -> DeploySchemaExtension:
    return DeploySchemaExtension(**kwargs)
