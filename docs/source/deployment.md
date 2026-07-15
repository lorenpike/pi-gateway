# Deployment and updates

**Documented wall-e version:** {sub-ref}`release`

Wall-e is distributed as a Linux Docker image at
`containers.metrized.com/wall-e:v<version>`. Use an immutable version tag in a
customer deployment; do not deploy `latest`.

## Prepare a new computer

Install Docker, then authenticate to the private registry:

```sh
docker login containers.metrized.com
```

Create a private deployment directory containing an `.env` file. At minimum it
needs a gateway token and provider access:

```ini
WALLE_TOKEN=replace-with-a-long-random-value
WALLE_PORT=6007
LANG=C.UTF-8
LC_ALL=C.UTF-8

# Use the credentials for the selected provider, or mount auth.json instead.
OPENROUTER_API_KEY=replace-me
```

Keep this directory out of source control. An existing pi `auth.json` and
`settings.json` may be placed beside `.env`; `deploy.sh` mounts either
file read-only when present. See [Environment variables](environment) for all
settings and credential choices.

Channel configuration is optional:

- [Telegram setup](channels/telegram)
- [Discord setup](channels/discord)
- [HTTP API](channels/http)

Use channel allowlists for customer deployments. The Telegram and Discord pages
explain how to obtain the IDs.

## First run or update

Download `deploy.sh`, `backup-home.sh`, and `restore-home.sh` from the matching
GitHub release assets (or obtain them from the same source tag as the image),
then restrict their permissions:

```sh
chmod 700 deploy.sh backup-home.sh restore-home.sh
```

Run `deploy.sh` from the private deployment directory. Replace `<version>` with
the desired release (the version documented by this page is shown above):

```sh
./deploy.sh \
  containers.metrized.com/wall-e:v<version> \
  .env .
```

The script pulls the image before replacing anything, recreates the `wall-e`
container, and preserves `/home/wall-e` in the `walle--home` named volume. It
uses restart policy `unless-stopped` and publishes:

- gateway and session UI: `http://localhost:6007/`
- bundled docs: `http://localhost:6080/wall-e/docs/`

Verify every deployment:

```sh
curl http://localhost:6007/health
docker exec wall-e wall-e --version
docker logs --tail 100 wall-e
```

To apply `.env`, auth, or settings changes, rerun the same command with the same
image tag. To update, rerun it with the new immutable tag. The script prints the
previous image; rollback by rerunning with that old tag.

The equivalent container lifecycle is always: pull, stop/remove the container,
and run the replacement with the same environment, mounts, ports, and named
home volume. Removing a container does **not** remove `walle--home`. Running
`docker volume rm walle--home` does and permanently deletes its contents.

## Back up the customer home volume

`walle--home` contains sessions, media, projects, cron state, Composio state,
and other customer files. Stop wall-e while archiving so the snapshot is
consistent:

```sh
./backup-home.sh ./backups
```

The script stops and restarts a running `wall-e` container and creates:

```text
walle-home--<UTC timestamp>.tar.gz
walle-home--<UTC timestamp>.tar.gz.sha256
walle-home--<UTC timestamp>.tar.gz.manifest
```

The manifest records the image, wall-e version, volume, time, and checksum. The
archive may contain customer conversations, uploaded media, projects, OAuth
state, or credentials. Encrypt it at rest and in transit, restrict access, and
apply the customer's retention policy.

The named-volume backup does **not** include bind-mounted `.env`, `auth.json`,
or `settings.json`. Back those up separately in the customer's protected secret
store. Never commit or attach them to a GitHub release.

Docker Desktop PowerShell equivalent (run while `wall-e` is stopped):

```powershell
$stamp = (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ')
docker stop wall-e
docker run --rm `
  -v walle--home:/source:ro `
  -v "${PWD}:/backup" `
  ubuntu:24.04 `
  tar --numeric-owner -C /source -czf "/backup/walle-home--$stamp.tar.gz" .
docker start wall-e
Get-FileHash -Algorithm SHA256 ".\walle-home--$stamp.tar.gz"
```

Record the running image (`docker inspect --format '{{.Config.Image}}' wall-e`)
with a PowerShell backup and keep that record with its checksum.

## Move to another computer

1. Back up the source computer and securely transfer the archive, checksum,
   manifest, `.env`, and any auth/settings files.
2. Install Docker and log in to the registry on the destination.
3. Verify the transferred SHA-256.
4. Restore into a new, empty volume:

   ```sh
   ./restore-home.sh \
     ./backups/walle-home--<timestamp>.tar.gz \
     walle--home-restored
   ```

   Docker Desktop PowerShell equivalent, after comparing `Get-FileHash` with
   the transferred checksum:

   ```powershell
   docker volume create walle--home-restored
   docker run --rm `
     -v walle--home-restored:/target `
     -v "${PWD}:/backup:ro" `
     ubuntu:24.04 `
     tar --numeric-owner -C /target -xzf /backup/walle-home--<timestamp>.tar.gz
   ```

   Use a new volume name: this manual command does not perform the restore
   script's non-empty-volume guard.

5. Start the **exact image tag in the manifest** against that volume:

   ```sh
   WALLE_HOME_VOLUME=walle--home-restored \
     ./deploy.sh containers.metrized.com/wall-e:v<version> .env .
   ```

6. Verify health, version, session history, projects, connected tools, and
   scheduled jobs. Only upgrade the image after the restored version works.

`restore-home.sh` verifies the checksum and refuses a destination that is
non-empty or attached to any container. Use a new volume rather than
overwriting the old one. `FORCE_RESTORE=1` is an
explicit destructive recovery option and erases the selected destination
before extracting; it never removes the backup archive or another volume.

Test restoration periodically into a temporary volume and container. A backup
that has never been restored is not a verified customer backup. After testing,
remove only the temporary container and volume whose names you confirmed.
