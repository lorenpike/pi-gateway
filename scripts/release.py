#!/usr/bin/env python3
"""Generate a GitHub release body from an exact wall-e changelog version."""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
VERSION_FILE = ROOT / "src" / "version" / "VERSION"
CHANGELOG = ROOT / "CHANGELOG.md"
DOCS_ROOT = "https://files.metrized.com/private/docs/wall-e"
IMAGE_ROOT = "containers.metrized.com/wall-e"
SEMVER_RE = re.compile(r"^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$")


def release_notes(text: str, version: str) -> str:
    heading = re.compile(
        rf"^## \[{re.escape(version)}\] - \d{{4}}-\d{{2}}-\d{{2}}\s*$",
        re.MULTILINE,
    )
    match = heading.search(text)
    if match is None:
        raise ValueError(f"CHANGELOG.md has no release section for {version}")
    end_match = re.search(r"^## \[", text[match.end() :], re.MULTILINE)
    end = match.end() + end_match.start() if end_match else len(text)
    notes = text[match.end() : end]
    notes = re.split(r"^\[Unreleased\]:", notes, maxsplit=1, flags=re.MULTILINE)[0]
    return notes.strip()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("version", help="release version, without a leading v")
    args = parser.parse_args()
    version = args.version.removeprefix("v")

    try:
        if SEMVER_RE.fullmatch(version) is None:
            raise ValueError(f"invalid semantic version: {args.version!r}")
        source_version = VERSION_FILE.read_text(encoding="utf-8").strip()
        if source_version != version:
            raise ValueError(
                f"tag version {version} does not match src/version/VERSION {source_version}"
            )
        notes = release_notes(CHANGELOG.read_text(encoding="utf-8"), version)
    except (OSError, ValueError) as error:
        print(f"release: {error}", file=sys.stderr)
        return 1

    docs = f"{DOCS_ROOT}/v{version}"
    print(notes)
    print(
        f"""

### Deployment

```sh
docker pull {IMAGE_ROOT}:v{version}
```

- [Documentation]({docs}/)
- [Deployment, updates, and backup]({docs}/deployment.html)
- Channel setup: [Telegram]({docs}/channels/telegram.html), [Discord]({docs}/channels/discord.html), [HTTP]({docs}/channels/http.html)
""".rstrip()
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
