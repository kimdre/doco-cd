"""Markdown extension shim bridging Zensical's link resolution with markdown-exec.

Zensical's ``zensical.extensions.preview`` extension (used to render hover
previews on internal links) requires a ``zensical.extensions.links.LinksTreeprocessor``
(registered under the name ``zrelpath``) to already be present on the
``markdown.Markdown`` instance it runs on. Zensical registers that
treeprocessor manually on the *page-level* Markdown instance, outside of the
normal extension list (see ``zensical.markdown.render.render``).

The ``markdown-exec`` plugin renders some fence languages (e.g. ``tree``) by
building its own, separate "mimicked" Markdown instance
(``markdown_exec._internal.rendering._mimic``) from the same extension list,
but it has no way to know about - and therefore never registers - Zensical's
manually-added ``LinksTreeprocessor``. When ``zensical.extensions.preview`` is
enabled, this causes the nested conversion to fail with::

    ValueError: No item named "zrelpath" exists.

pymdownx.superfences silently swallows that error, which manifests as
"tree" fences (and any other markdown-exec fence relying on Markdown output)
being rendered as unprocessed plain text instead of the expected HTML.

This extension registers a no-op fallback ``LinksTreeprocessor`` on any
Markdown instance that doesn't already have one, so nested/mimicked instances
created by markdown-exec always have a working ``zrelpath`` treeprocessor
available. It is a no-op for normal page rendering, since Zensical's own
``LinksExtension`` - registered afterwards, with the correct page path -
simply replaces it (Markdown's treeprocessor registry replaces items
registered under the same name).
"""

from __future__ import annotations

from typing import Any

from markdown import Extension, Markdown
from zensical.extensions.links import LinksTreeprocessor


class ExecLinksShimExtension(Extension):
    """Ensure a `zrelpath` treeprocessor is always registered."""

    name = "hooks.exec_links_shim"

    def extendMarkdown(self, md: Markdown) -> None:
        """Register a fallback LinksTreeprocessor if none is present yet."""
        md.registerExtension(self)
        if LinksTreeprocessor.name in md.treeprocessors:
            return
        # Path/use_directory_urls don't matter here: this fallback only
        # exists to satisfy zensical.extensions.preview's requirement that a
        # LinksTreeprocessor is registered. It never runs for real pages,
        # since those get a proper LinksTreeprocessor registered by Zensical
        # itself, which replaces this one (same registry name: "zrelpath").
        processor = LinksTreeprocessor(md, path="", use_directory_urls=True)
        md.treeprocessors.register(processor, processor.name, 0)


def makeExtension(**kwargs: Any) -> ExecLinksShimExtension:
    """Register the Markdown extension."""
    return ExecLinksShimExtension()
