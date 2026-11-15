# Releasing

Shipping a version is pushing a tag. GitHub builds the release; every `maestria` and `maestria-cloud` that checks for updates picks it up. GitHub never holds credentials to any server: machines pull, nothing pushes.

## How updates reach people

```mermaid
flowchart LR
  T["git tag vX.Y.Z<br/>git push origin vX.Y.Z"] --> W["release workflow<br/>archives + SHA256SUMS"]
  W --> R["GitHub release"]
  R -- "every ~15 min (systemd timer)" --> V["the hosted VM<br/>maestria-cloud update --yes"]
  R -- "when you run it" --> U["maestria update"]
  R -- "once a day, a note only" --> P["panel: update available"]
```

- **Hosted VM:** a systemd timer runs `maestria-cloud update --yes` about every 15 minutes (installed by `packaging/linux/setup-cloud.sh`, opt out with `--no-auto-update`). A new tag reaches the VM within roughly 15 to 20 minutes.
- **Everyone else:** `maestria update` installs the latest release. The panel only *tells* people one exists (a line at startup and a dismissible note on the Security page, at most one request to GitHub a day); it never installs by itself. `"updates": {"check": false}` in `config.json` turns the check off.

What an update does, in order: finds the newest release (GitHub's public API, no token), downloads the archive for this OS and CPU, **checks its SHA-256 against `SHA256SUMS`** (a mismatch or a missing entry stops everything), unpacks only the expected binary, runs it with `version`, replaces the old binary atomically, keeps the old one as `<name>.previous`, restarts the service if one runs, and waits up to 30 seconds for it to be healthy. If it is not, the previous binary is put back and the service restarted again. Development builds (`2.0.0-dev+abc1234`, or built without a version) never update by themselves, and nothing downgrades without `--force`.

The checksum file comes from the same GitHub release as the archive, so it protects against a damaged or swapped download, not against someone who can publish to the repository. Protect the repository (branch protection, two-factor sign-in, limited access to tags).

## Cut a release

1. On `main`, with CI green, move the entries of **Unreleased** in [`CHANGELOG.md`](../CHANGELOG.md) under a new heading, `## [X.Y.Z] - YYYY-MM-DD`, and leave an empty **Unreleased** above it.
2. Commit it (`chore: release vX.Y.Z`).
3. Tag and push the tag:

   ```sh
   git tag vX.Y.Z
   git push origin main vX.Y.Z
   ```

   Versions are [semver](https://semver.org): `vMAJOR.MINOR.PATCH`. The tag must start with `v`.
4. Watch the **Release** workflow (`.github/workflows/release.yml`). It builds `maestria` and `maestria-cloud` for macOS (arm64, amd64), Linux (amd64, arm64) and Windows (amd64), writes `SHA256SUMS` and the Homebrew formula, and attaches them to a GitHub release.
5. Check the release page: for every binary and target there is one archive named `<binary>_<version>_<os>_<arch>.tar.gz` (`.zip` on Windows), plus `SHA256SUMS` and `maestria.rb`.
6. Check that it arrived, from any machine that has the previous version:

   ```sh
   maestria update --check
   ```

   On the VM, the timer does the rest. To see it happen: `journalctl -u maestria-cloud-update -n 30`, and `maestria-cloud version`.

The asset names are produced by one function in `internal/update` (`ArchiveName`) that both `tools/release` and the updater use, and a test builds a real release and installs it through the updater, so the two cannot drift apart.

## Cut a prerelease

Use a semver prerelease tag, for example `v1.3.0-rc.1`, and push it as above. The workflow publishes any tag with a hyphen as a GitHub **pre-release** on its own. GitHub's "latest release" ignores prereleases, so the VM timer and a plain `maestria update` do not see it.

To try it on a machine you control:

```sh
maestria update --channel prerelease
```

The prerelease channel picks the highest version among all published releases, so once `v1.3.0` ships, that channel follows it.

## Roll back

A bad release should not stay the newest one. In order of speed:

1. **A machine that just updated and failed its health check** already rolled itself back. Nothing to do.
2. **Stop the spread:** on the release page, mark the bad release as a **pre-release** or **delete** it (keep the tag if you want to look at it). GitHub's "latest" then points to the previous release, and machines that have not updated yet stay where they are. If you delete the release but its tag still exists, delete the tag too (`git push origin :refs/tags/vX.Y.Z`) so it cannot be re-published by accident.
3. **Move machines that already updated back.** Updates never go backwards by themselves, so say so:

   ```sh
   sudo maestria-cloud update --yes --force --version vX.Y.Z    # on the VM
   maestria update --yes --force --version vX.Y.Z               # on a computer
   ```

   `--version` names the tag to install (an older one is fine with `--force`). Without `--version`, `--force` reinstalls the latest release.
4. **Right after an update**, the old binary is still next to the new one: `<name>.previous` (for example `/usr/local/bin/maestria-cloud.previous`). Copy it back over the binary and restart the service if you cannot reach GitHub.
5. Ship a fixed `vX.Y.(Z+1)`. Its tag is newer, so every machine moves forward to it.

Mind the database on the hosted server: a new version may change the schema. Before replacing the binary, `maestria-cloud update` runs the daily backup script (`/usr/local/sbin/maestria-cloud-backup`), so there is a copy from just before the update in `/var/backups/maestria-cloud`. Restoring is covered in [Run your own hosted server](deploy-cloud.md#10-back-up).

## Turn updates off

| Where | How |
| --- | --- |
| Hosted VM | Re-run `setup-cloud.sh` with `--no-auto-update`, or `sudo systemctl disable --now maestria-cloud-update.timer`. |
| The panel's notice | `"updates": {"check": false}` in `config.json`. |
