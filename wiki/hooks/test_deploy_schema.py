"""Tests for the deployment schema documentation extension."""

from __future__ import annotations

import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from markdown import Markdown

from hooks import deploy_schema

SCHEMA = b'{"$schema":"http://json-schema.org/draft-07/schema#","type":"object"}\n'


class DeploySchemaTests(unittest.TestCase):
    def setUp(self) -> None:
        deploy_schema._generate_schema.cache_clear()

    def tearDown(self) -> None:
        deploy_schema._generate_schema.cache_clear()

    def test_generator_is_cached_until_inputs_change(self) -> None:
        result = subprocess.CompletedProcess([], 0, stdout=SCHEMA, stderr=b"")
        with patch.object(deploy_schema.subprocess, "run", return_value=result) as run:
            self.assertEqual(deploy_schema._generate_schema("first"), SCHEMA)
            self.assertEqual(deploy_schema._generate_schema("first"), SCHEMA)
            self.assertEqual(run.call_count, 1)
            self.assertEqual(deploy_schema._generate_schema("second"), SCHEMA)
            self.assertEqual(run.call_count, 2)

    def test_generator_failures_are_reported(self) -> None:
        failures = (
            (FileNotFoundError(), "requires Go"),
            (subprocess.CalledProcessError(1, "go", stderr=b"generation failed"), "generation failed"),
            (subprocess.TimeoutExpired("go", 120), "120-second timeout"),
        )
        for error, message in failures:
            with self.subTest(error=error), patch.object(deploy_schema.subprocess, "run", side_effect=error):
                with self.assertRaisesRegex(RuntimeError, message):
                    deploy_schema._generate_schema("input")

    def test_invalid_generator_output_is_rejected(self) -> None:
        for output in (b"", b"invalid", b"{}", b"[]"):
            result = subprocess.CompletedProcess([], 0, stdout=output, stderr=b"")
            with self.subTest(output=output), patch.object(deploy_schema.subprocess, "run", return_value=result):
                with self.assertRaises(RuntimeError):
                    deploy_schema._generate_schema("input")

    def test_fingerprint_tracks_sources_and_dependencies(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "go.mod").write_text("module example\n")
            (root / "go.sum").write_text("")
            source = root / "cmd/deploy-schema/main.go"
            source.parent.mkdir(parents=True)
            source.write_text("package main\n")
            with patch.object(deploy_schema, "REPOSITORY_ROOT", root):
                initial = deploy_schema._source_fingerprint()
                (source.parent / "main_test.go").write_text("package main\n")
                self.assertEqual(initial, deploy_schema._source_fingerprint())
                source.write_text("package main\n// changed\n")
                self.assertNotEqual(initial, deploy_schema._source_fingerprint())
                changed = deploy_schema._source_fingerprint()
                (root / "go.sum").write_text("changed dependency\n")
                self.assertNotEqual(changed, deploy_schema._source_fingerprint())

    def test_fingerprint_tracks_doco_cd_version(self) -> None:
        with patch.dict("os.environ", {"DOCO_CD_VERSION": "v1.2.3"}):
            first = deploy_schema._source_fingerprint()
        with patch.dict("os.environ", {"DOCO_CD_VERSION": "v1.2.4"}):
            second = deploy_schema._source_fingerprint()
        self.assertNotEqual(first, second)

    def test_extension_updates_source_and_built_asset_without_rewriting(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "docs/schemas/deploy.schema.json"
            site = root / "output/schemas/deploy.schema.json"
            config = {"root_dir": str(root), "site_dir": "output"}
            with (
                patch.object(deploy_schema, "SCHEMA_PATH", source),
                patch("hooks.deploy_schema.get_config", return_value=config),
                patch.object(deploy_schema, "_source_fingerprint", return_value="inputs"),
                patch.object(deploy_schema, "_generate_schema", return_value=SCHEMA),
            ):
                deploy_schema.makeExtension().extendMarkdown(Markdown())
                self.assertEqual(source.read_bytes(), SCHEMA)
                self.assertEqual(site.read_bytes(), SCHEMA)
                source_mtime = source.stat().st_mtime_ns
                site_mtime = site.stat().st_mtime_ns
                deploy_schema.makeExtension().extendMarkdown(Markdown())
                self.assertEqual(source.stat().st_mtime_ns, source_mtime)
                self.assertEqual(site.stat().st_mtime_ns, site_mtime)
                site.write_bytes(b"stale")
                deploy_schema.makeExtension().extendMarkdown(Markdown())
                self.assertEqual(site.read_bytes(), SCHEMA)
