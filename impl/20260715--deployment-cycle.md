# Portable release and deployment cycle — Implementation Plan

**Date:** 2026-07-15  
**Status:** Implemented

## Goal

Publish wall-e as a SemVer-tagged Docker image plus version-matched HTML docs,
create a GitHub release in `millie-research-inc/wall-e`, and give an operator a
repeatable way to replace or roll back the container without losing
`/home/wall-e`.

Release builds target `linux/amd64` for x64 Windows/Linux Docker hosts.

## Version and changelog

- Add `src/version/VERSION` containing only the SemVer value (initial baseline:
  `0.1.0`). This is the single source of truth.
- Add `src/version/version.go`, embedding that file and exposing the normalized
  value. `src/main.go` will print `wall-e <version>` for either `--version` or
  `-V`, without loading configuration. Add both flags to CLI usage and tests.
- `docs/source/conf.py`, Make recipes, image labels, and release scripts must
  read this file; do not copy the version into another source file.
- Add a Keep a Changelog-style `CHANGELOG.md` with `Unreleased` and comparison
  links rooted at:
  `https://github.com/millie-research-inc/wall-e`.
- Add `scripts/bump.py <major|minor|patch>`. It will validate SemVer, allow the
  expected edited `CHANGELOG.md` but reject unrelated working-tree changes,
  update `src/version/VERSION`, promote `Unreleased` to a dated release, and
  rewrite comparison links. Its printed Git commands must use the `millie`
  remote, never `origin`.

Use the structure of these local examples, but remove their package-specific
assumptions and bump-counter call:

- `M:/Metrized/aligned-vision/metrized-cv/scripts/bump.py`
- `M:/Metrized/scripts/ncv/scripts/bump.py`
- `M:/Metrized/aligned-vision/laserpath/CHANGELOG.md`

## Publishing

Extend the root `Makefile` with overridable deployment settings and a `push`
target:

```make
REGISTRY ?= containers.metrized.com
RELEASE_IMAGE := $(REGISTRY)/wall-e:v$(VERSION)
DOCS_HOST ?= metrized_server_0@10.0.0.5
DOCS_ROOT ?= C:/Metrized/metrized-files/files/private/docs/wall-e
```

`make push` will:

1. require a clean `main` checkout and registry/static-server access;
2. run Go tests and a warning-as-error Sphinx build;
3. build explicitly for `linux/amd64`, with OCI version/source/revision labels;
4. smoke-test `wall-e --version` inside the image;
5. push immutable `v<version>`, then update `latest` only after the immutable
   push succeeds;
6. upload docs to `$(DOCS_ROOT)/v<version>/`, then update the current docs at
   `$(DOCS_ROOT)/`.

A separate `make docs` target builds and publishes only the current docs so
operators can iterate on documentation without rebuilding or pushing the image.

Use the registry/static-host shape from
`M:/Metrized/aligned-vision/metrized-cv/Makefile`; do not copy embedded
credentials from its older deployment scripts.

Add `scripts/release.py <version>` to extract the exact changelog section and
append these version-correct links:

- `https://files.metrized.com/private/docs/wall-e/v<version>/`
- `docker pull containers.metrized.com/wall-e:v<version>`
- the deployment page and Telegram, Discord, and HTTP channel pages under that
  versioned docs URL.

Add `.github/workflows/on-release.yaml`, triggered by `vX.Y.Z` tags. It checks
that the tag, `src/version/VERSION`, and changelog agree, generates the body
with `scripts/release.py`, and creates the GitHub release. The workflow only
creates the GitHub release; private image/docs publication remains the local
`make push` step. Replace existing hard-coded `lorenpike/pi-gateway` links in
channel docs with `millie-research-inc/wall-e` links.

## Release procedure

Document this exact order in a short release section:

1. Edit `CHANGELOG.md` under `Unreleased`.
2. Run `uv run scripts/bump.py <patch|minor|major>` and review the diff.
3. Commit the bump, merge it to `main`, and switch to the resulting clean
   `main` checkout.
4. Run `make push`. Do not tag a release whose artifacts failed to publish.
5. Create `v<version>` at that commit and run
   `git push millie main v<version>`. The tag starts the GitHub workflow.
6. Verify the GitHub release, versioned docs, registry tag, and
   `docker run --rm <image> wall-e --version`.

This follows `docs/source/release.rst` in the local `metrized-cv` example while
making the artifact-first ordering and `millie` remote explicit.

## Portable container operation

Add `docs/source/deployment.md` and link it from `docs/source/index.md`. Add a
small `scripts/deploy.sh` based on the lifecycle—not the credentials—in
`M:/Metrized/aligned-vision/metrized-cv/scripts/rebuild-v2.sh`.

The script must require an explicit immutable image tag, pull it before touching
the running container, then recreate `wall-e` with:

- `--env-file <deployment .env>`;
- `--restart unless-stopped`;
- `${WALLE_HOME_VOLUME:-walle--home}:/home/wall-e` so sessions, cron state, and
  user files survive and a restored volume can be selected explicitly;
- optional read-only host `auth.json` and `settings.json` mounts at
  `/opt/pi/auth.json` and `/opt/pi/settings.json`;
- ports `6007:6007` and `6080:80`.

The deployment page must include first run, upgrade, rerun after an `.env`
change, rollback to the previous immutable tag, `docker logs`, `/health`,
`wall-e --version`, and a warning that `docker rm` does not remove the named
volume (while `docker volume rm walle--home` does).

### Home-volume backup and computer migration

Treat backup/restore as part of the supported customer deployment, not an
advanced Docker aside. Add `scripts/backup-home.sh` and
`scripts/restore-home.sh`, plus equivalent Docker Desktop/PowerShell commands in
the deployment page.

The documented procedure must:

1. stop `wall-e` before archiving so transcripts, cron state, and other files
   form a consistent snapshot;
2. archive the *contents* of the named home volume through a temporary Ubuntu
   container into `walle-home--<UTC timestamp>.tar.gz`;
3. write a small manifest containing the wall-e image/version, creation time,
   volume name, and archive SHA-256, then verify the checksum;
4. explain that the archive contains customer conversations, media, projects,
   cron files, and possibly credentials, so it must be encrypted and handled as
   sensitive customer data;
5. back up `.env`, `auth.json`, and `settings.json` separately—the read-only
   host files are not stored in `walle--home`—without putting them into a GitHub
   release or source control;
6. transfer the archive, manifest, and separately protected configuration to
   the new computer, verify SHA-256, restore as root into a newly created empty
   volume, and preserve the numeric ownership/modes from the tar archive;
7. start the exact recorded image tag against the restored volume, verify
   `/health`, `wall-e --version`, sessions, and scheduled jobs, and only then
   upgrade to a newer image.

Restore tooling must refuse a non-empty destination volume unless the operator
passes an explicit destructive override; it must never delete the source or
existing destination volume. The docs should also include a periodic restore
smoke test, because an untested archive is not a customer backup.

Keep variable details canonical in `docs/source/environment.md`. The deployment
page should summarize only:

- required: `WALLE_TOKEN`;
- required provider access: an appropriate API key or mounted pi `auth.json`;
- optional channel secrets/allowlists: `WALLE_TELEGRAM_*` and
  `WALLE_DISCORD_*`;
- optional tool/tunnel credentials such as `BRAVE_API_KEY` and
  `CLOUDFLARE_TOKEN`.

Link directly to `docs/source/channels/telegram.md`, `discord.md`, and `http.md`
for channel setup rather than duplicating it. Secrets stay in the ignored
`.env`/deployment secret store and must not appear in shell history, images, or
GitHub release assets.

## Validation

- `wall-e --version` and `wall-e -V` are identical and work without env vars.
- The embedded version, changelog release, Git tag, OCI label, image tag, docs
  version, and GitHub release all match.
- `make push` cannot publish from dirty/non-`main` state or overwrite an
  immutable version with different content.
- Recreating with a newer and then older image preserves a sentinel file and
  sessions in `walle--home`.
- Backup/restore round-trip preserves file contents, permissions, session
  history, cron configuration, and a checksum; restore refuses a populated
  destination volume.
- Fresh-host smoke: registry login, verified home archive, `.env` plus provider
  auth, `deploy.sh`, `/health`, bundled docs, and one configured channel all
  work without a source build checkout.

## Implementation log

Implemented on 2026-07-15 with version `0.1.0` as the first release baseline.

- Added the embedded single source at `src/version/VERSION`, CLI version flags,
  tests, `CHANGELOG.md`, bump/release scripts, and the tag-triggered GitHub
  workflow.
- Added release/image/docs targets to `Makefile`; release images are
  `containers.metrized.com/wall-e:v<version>`, docs read the same source
  version, and generated stamp files are hidden (`build/.docker-stamp`).
- Added `docs/source/release.md` and `docs/source/deployment.md`, including
  channel links, environment guidance, updates, rollback, customer backups,
  restore testing, and computer migration.
- Added deploy, backup, and guarded restore scripts. Restore requires a verified
  checksum and refuses a populated destination unless explicitly overridden.
- Pinned the Composio CLI build input after its latest-release API lookup made a
  clean Docker build non-deterministic.

Validation completed: Go test/vet passed, Sphinx passed with warnings as errors,
release/bump scripts passed a temporary-repository release test, the Docker
image built as `linux/amd64` and reported `wall-e 0.1.0` with matching OCI
labels, and a live temporary-volume backup/restore round trip preserved content
and mode while refusing a second restore into the populated volume. Registry,
static-server, and GitHub publication were intentionally not run before review.
