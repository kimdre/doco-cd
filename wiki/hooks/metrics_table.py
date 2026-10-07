"""Render the available Prometheus metrics as a Markdown table."""

from __future__ import annotations

import subprocess
from functools import lru_cache
from hashlib import sha256
from pathlib import Path
from typing import Any

from markdown import Extension, Markdown
from markdown.preprocessors import Preprocessor

METRICS_PLACEHOLDER = "<!-- metrics-table -->"
REPOSITORY_ROOT = Path(__file__).resolve().parents[2]
GENERATOR_INPUTS = (
    "cmd/metrics-docs/main.go",
    "internal/prometheus/collectors.go",
    "internal/prometheus/metrics.go",
)


def _source_fingerprint() -> str:
    digest = sha256()
    for relative_path in GENERATOR_INPUTS:
        digest.update(relative_path.encode())
        digest.update(b"\0")
        digest.update((REPOSITORY_ROOT / relative_path).read_bytes())
    return digest.hexdigest()


@lru_cache(maxsize=1)
def _generate_metrics_markdown(_fingerprint: str) -> str:
    """Cache generated Markdown until one of its source files changes."""
    try:
        result = subprocess.run(
            ["go", "run", "./cmd/metrics-docs"],
            cwd=REPOSITORY_ROOT,
            check=True,
            capture_output=True,
            text=True,
            timeout=120,
        )
    except FileNotFoundError as error:
        raise RuntimeError("The metrics reference requires Go to be installed and available on PATH") from error
    except subprocess.CalledProcessError as error:
        details = error.stderr.strip() or error.stdout.strip() or "no error output"
        raise RuntimeError(f"Could not generate the metrics table: {details}") from error
    except subprocess.TimeoutExpired as error:
        raise RuntimeError("Generating the metrics table exceeded the 120-second timeout") from error

    generated = result.stdout.strip()
    if not generated:
        raise RuntimeError("The metrics table generator returned no Markdown")
    return generated


class MetricsTablePreprocessor(Preprocessor):
    """Replace the metrics table marker with collector metadata."""

    def run(self, lines: list[str]) -> list[str]:
        matches = [index for index, line in enumerate(lines) if line.strip() == METRICS_PLACEHOLDER]
        if not matches:
            return lines
        if len(matches) > 1:
            raise RuntimeError("The metrics page must contain exactly one metrics-table placeholder")

        generated = _generate_metrics_markdown(_source_fingerprint())
        lines[matches[0] : matches[0] + 1] = generated.splitlines()
        return lines


class MetricsTableExtension(Extension):
    """Register the metrics table preprocessor."""

    def extendMarkdown(self, md: Markdown) -> None:
        md.preprocessors.register(MetricsTablePreprocessor(md), "metrics_table", 25)
        md.registerExtension(self)


def makeExtension(**kwargs: Any) -> MetricsTableExtension:
    return MetricsTableExtension(**kwargs)
