#!/usr/bin/env python3
"""Bump wall-e's single version source and promote CHANGELOG Unreleased notes."""

from __future__ import annotations

import argparse
import re
import subprocess
import sys
from dataclasses import dataclass
from datetime import date
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
VERSION_FILE = ROOT / "src" / "version" / "VERSION"
CHANGELOG = ROOT / "CHANGELOG.md"
REPOSITORY_URL = "https://github.com/millie-research-inc/wall-e"
SEMVER_RE = re.compile(r"^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$")
RELEASE_RE = re.compile(
    r"^## \[((?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*))\] - \d{4}-\d{2}-\d{2}$",
    re.MULTILINE,
)


@dataclass(frozen=True)
class Version:
    major: int
    minor: int
    patch: int

    @classmethod
    def parse(cls, value: str) -> "Version":
        match = SEMVER_RE.fullmatch(value)
        if match is None:
            raise ValueError(f"invalid semantic version: {value!r}")
        return cls(*(int(part) for part in match.groups()))

    def __str__(self) -> str:
        return f"{self.major}.{self.minor}.{self.patch}"

    def bump(self, kind: str) -> "Version":
        if kind == "major":
            return Version(self.major + 1, 0, 0)
        if kind == "minor":
            return Version(self.major, self.minor + 1, 0)
        return Version(self.major, self.minor, self.patch + 1)


def git(*args: str) -> str:
    result = subprocess.run(
        ["git", "-C", str(ROOT), *args],
        check=True,
        capture_output=True,
        text=True,
    )
    return result.stdout


def check_git() -> str:
    branch = git("branch", "--show-current").strip()
    if not branch:
        raise RuntimeError("detached HEAD is not a release branch")
    if branch == "main":
        raise RuntimeError("run the bump on a release branch, then merge it to main")

    unrelated: list[str] = []
    for line in git("status", "--porcelain").splitlines():
        path = line[3:]
        if " -> " in path:
            path = path.split(" -> ", 1)[1]
        if path.replace("\\", "/") != "CHANGELOG.md":
            unrelated.append(line)
    if unrelated:
        raise RuntimeError(
            "commit or stash changes other than CHANGELOG.md first:\n  "
            + "\n  ".join(unrelated)
        )
    return branch


def update_changelog(text: str, old: Version, new: Version) -> str:
    heading = "## [Unreleased]"
    if text.count(heading) != 1:
        raise ValueError("CHANGELOG.md must contain exactly one Unreleased heading")

    after_unreleased = text.split(heading, 1)[1]
    next_release = RELEASE_RE.search(after_unreleased)
    notes = after_unreleased[
        : next_release.start() if next_release else len(after_unreleased)
    ]
    notes = re.sub(r"^\[[^]]+\]: .*$", "", notes, flags=re.MULTILINE).strip()
    if not notes:
        raise ValueError("CHANGELOG.md Unreleased section is empty")

    releases = RELEASE_RE.findall(text)
    if releases and releases[0] != str(old):
        raise ValueError(
            f"latest changelog version {releases[0]} does not match VERSION {old}"
        )

    release_heading = f"## [{new}] - {date.today():%Y-%m-%d}"
    text = text.replace(heading, f"{heading}\n\n{release_heading}", 1)

    unreleased_ref = re.compile(r"^\[Unreleased\]: .+$", re.MULTILINE)
    if unreleased_ref.search(text) is None:
        raise ValueError("CHANGELOG.md is missing the Unreleased reference link")
    if releases:
        release_url = f"{REPOSITORY_URL}/compare/v{old}...v{new}"
    else:
        release_url = f"{REPOSITORY_URL}/releases/tag/v{new}"
    replacement = (
        f"[Unreleased]: {REPOSITORY_URL}/compare/v{new}...HEAD\n[{new}]: {release_url}"
    )
    return unreleased_ref.sub(replacement, text, count=1)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("kind", choices=("major", "minor", "patch"))
    parser.add_argument("-y", "--yes", action="store_true", help="skip confirmation")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    try:
        branch = check_git()
        old = Version.parse(VERSION_FILE.read_text(encoding="utf-8").strip())
        new = old.bump(args.kind)
        changelog = update_changelog(CHANGELOG.read_text(encoding="utf-8"), old, new)
    except (OSError, ValueError, RuntimeError, subprocess.CalledProcessError) as error:
        print(f"bump: {error}", file=sys.stderr)
        return 1

    print(f"wall-e {old} -> {new}")
    if not args.yes and input("Continue? [y/N]: ").strip().lower() != "y":
        return 0

    VERSION_FILE.write_text(f"{new}\n", encoding="utf-8")
    CHANGELOG.write_text(changelog, encoding="utf-8")
    print(
        f"""Updated {VERSION_FILE.relative_to(ROOT)} and CHANGELOG.md.

Review and release with:
  git add CHANGELOG.md src/version/VERSION
  git commit -m "Release v{new}"
  git switch main
  git merge {branch}
  make push
  git tag v{new}
  git push millie main v{new}
"""
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
