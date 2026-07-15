# Release process

Wall-e uses Semantic Versioning. `src/version/VERSION` is the single version
source used by the CLI, docs, image, changelog tooling, and release workflow.

The first `v0.1.0` baseline is already prepared in the version file and
changelog. After reviewing and merging this implementation, publish it starting
at step 4 below; do not bump it to `0.1.1` merely to make the first release.
The full procedure applies to subsequent releases.

## Publish a release

1. Add release notes under `Unreleased` in `CHANGELOG.md`.
2. From a release branch, run:

   ```sh
   uv run scripts/bump.py <patch|minor|major>
   ```

3. Review and commit `CHANGELOG.md` and `src/version/VERSION`, merge to `main`,
   and switch to the clean `main` checkout.
4. Publish the tested image and versioned documentation:

   ```sh
   make push
   ```

5. Only after publication succeeds, tag the same commit and push through the
   company GitHub remote:

   ```sh
   git tag v<version>
   git push millie main v<version>
   ```

The tag starts `.github/workflows/on-release.yaml`, which validates the tag
against the source version and changelog and creates the GitHub release. Use
`millie`, not `origin`; `origin` points at the upstream gateway repository.

## Publish docs while iterating

To build and upload only the current docs to the unversioned/latest docs URL:

```sh
make docs
```

For a local-only preview, use `cd docs && make html` or `cd docs && make dev`.
`make push` additionally publishes the immutable `v<version>` docs tree used by
the corresponding GitHub release.
