# t3-update-preview

Update enrolled T3 Code preview installations with one command:

```sh
t3-update-preview
```

The updater selects one published preview, prepares all selected devices, then updates remote devices before the local device. It checks the CLI, managed server, and selected desktop app. It keeps recovery data on each device.

This is an independent project. It is not an official T3 Code updater. Read the [connection compatibility](docs/compatibility.md) before enrolling an installation. Verification uses isolated fixtures; no live private devices were updated to test this project.

## Install

Use macOS or Linux on arm64 or amd64. Each selected device needs Python 3.10 or later, a user-owned standalone T3 preview CLI, and write access to the selected installation. Standalone server updates need T3's managed user service. Linux service management needs a working user systemd session. Remote devices need OpenSSH access as the installation owner.

Download the archive for your OS and CPU from [Releases](https://github.com/Bil0000/t3-update-preview/releases). Archives use the name `t3-update-preview-VERSION-OS-ARCH.tar.gz`, where OS is `darwin` or `linux` and ARCH is `arm64` or `amd64`. Check the archive against the release's `SHA256SUMS` before extracting it. Put the executable in a directory on your `PATH`.

To build from source, use Go 1.24 or later:

```sh
git clone https://github.com/Bil0000/t3-update-preview.git
cd t3-update-preview
go build -trimpath -o dist/t3-update-preview ./cmd/t3-update-preview
./dist/t3-update-preview --version
./dist/t3-update-preview --setup
```

Keep the executable at a fixed path if you enable scheduled notices. Setup also saves your `PATH` and adds the standard system tool directories to the job. Empty or relative `PATH` entries block schedule installation. Run setup again after moving the executable or changing its tool paths.

## Installation support

These routes have isolated fixture tests. Live app replacement, OS sleep/wake, and authenticated relay sessions are not certified by those tests.

| Installation | Requirements |
| --- | --- |
| Standalone preview CLI | A user-owned archive install on macOS or Linux. |
| Managed server | The selected user account manages its T3 launchd or systemd service, with verified data and health paths. |
| macOS preview app | The signed T3 app, a standalone CLI, and permission to quit and reopen it. Supports an embedded server or a managed service. |
| Linux preview desktop | A user-owned type-2 AppImage, a standalone CLI, and a desktop session to reopen the app. |
| Package-managed installation | Blocked. Use the package manager to update its preview. |
| Direct or relay connection | An explicit SSH recovery alias and a matching public T3 connection descriptor. |

Linux discovery checks `~/Applications` and `~/.local/bin` for T3 Code AppImages. Use `app_path` for a different location or to select between multiple copies.

Desktop updates preserve the selected path. AppImages also retain their file mode. macOS app replacement keeps supported Finder custom-icon metadata. An unsupported CPU, missing asset, unknown installation, or missing permission stops the affected update.

## First setup

```sh
t3-update-preview --setup
```

Setup offers the local device, saved devices, and explicit aliases from your SSH configuration. Choose the devices to enroll, saved exclusions, and whether to enable hourly notices. Press Enter to keep a saved choice. Enter `none` to clear devices or exclusions. Setup verifies each selected device and saves its identity. New discoveries require another explicit enrollment choice.

Discovery reads SSH configuration without connecting to its hosts or executing `Match exec` commands. It does not scan networks or expand wildcard hosts. When you select a remote device, OpenSSH uses your existing alias, keys, proxy settings, and known host keys. Password prompts and unknown host keys stop the operation. Establish trusted SSH access yourself before enrollment.

For explicit installation paths or connection routes, provide a JSON array:

```json
[
  {
    "id": "work-server",
    "name": "Work server",
    "kind": "ssh",
    "host": "work-server"
  }
]
```

Save this as `devices.json`, then run:

```sh
t3-update-preview --setup --import devices.json
```

`host` is an existing SSH alias. Optional `base_dir`, `cli_path`, and `app_path` fields must be absolute paths on that device. Use `app_path` to select one app when several copies exist. An optional `health_url` must be the selected server's loopback HTTP or HTTPS address. Do not add passwords, tokens, or keys to the import file.

Direct and relay records use `kind: "direct"` or `kind: "relay"`, a `url`, and an explicit SSH recovery alias in `host`. Their public connection descriptor must match the enrolled server's identity and version before and after an update. This checks endpoint reachability and version, not authenticated T3 RPC or a Clerk session.

The updater uses SSH for installation and recovery. A T3 connection alone does not grant shell access. Authenticated-only descriptors and routes without an SSH recovery path are blocked. It does not extract encrypted app credentials. See [connection requirements](docs/compatibility.md).

## Use

| Command | Action |
| --- | --- |
| `t3-update-preview` | Update selected installations to one preview. |
| `t3-update-preview --check` | Check for a newer preview without installing it. |
| `t3-update-preview --status` | Inspect enrolled versions, activity, and recovery state. |
| `t3-update-preview --dry-run` | Inspect the selected release and devices without replacing T3. |
| `t3-update-preview --exclude local --exclude work-server` | Omit these enrolled devices from this run. |
| `t3-update-preview --wait 10m` | Wait up to ten minutes for idle devices. |
| `t3-update-preview --force` | Allow interruption of active work during an update. |
| `t3-update-preview --rollback work-server` | Restore one device from its retained backup, if safe. |
| `t3-update-preview --help` | Show all flags and default paths. |

Exclusions accept an enrolled ID or unique name. They apply before network contact. Saved aliases for the same verified installation are excluded together. Unknown or ambiguous names stop the command. Setup can save exclusions for later runs.

The default idle wait is 30 minutes. Active database runs and unrecognized child jobs defer an update. Open terminal or provider processes can keep a device busy after a turn ends. The updater checks activity again before stopping T3. Unknown activity blocks a normal update. There is no atomic maintenance gate to prevent new work after that check. `--force` permits interruption when you accept it; ownership, identity, download, disk-space, backup, and health checks still apply.

A desktop update must close and reopen the app. A server update must restart the service. Neither can be swapped into a running process without a restart. Active turns and terminal jobs can stop. Run the updater from an independent terminal so closing T3 does not close its coordinator.

All required release files must exist for the selected devices. The updater checks release digests, archive paths, CPU and version, and macOS app signatures. It refuses automatic downgrades and unsupported installation routes. Package-managed installs stay with their package manager; this tool does not replace their files.

If preparation fails, activation does not start. If activation or health checks fail, later devices remain unchanged. Devices already verified on the new preview remain there. Inspect `--status` before retrying.

## Hourly notices

Enable or disable notices through `--setup`. Background jobs check and notify only. They never install an update or restart T3.

macOS uses a user LaunchAgent. Linux uses a user systemd timer. Hourly, login, wake, and retry triggers share a process lock and saved due time. Ten missed hours produce one catch-up check. Failed requests and failed notice commands retry after five minutes. Repeated checks for the same release do not repeat a successful notice.

The notice says:

> T3 preview VERSION is available. Run t3-update-preview to update.

On macOS, allow notifications for the process that runs `osascript` in System Settings. Desktop control can also need Automation permission. On Linux, notices need `notify-send`, a desktop notification service, and a user session with access to it. A headless server can use `--check` and `--status` without desktop notices. Scheduler setup errors leave saved enrollment available for manual commands.

The OS can suppress a notice even if its command succeeds. A crash after delivery but before saving delivery state can repeat it. Real sleep/wake timing and desktop permission behavior require verification in your own OS session.

## Backups and recovery

Each device stores updater operations beneath `base_dir/update-preview/`. Each operation has a private journal and recovery files. `--status` reports the retained backup location. Backups include affected SQLite data, settings, CLI/runtime files, service configuration, and the selected desktop app. SQLite snapshots are taken after the affected processes stop.

The previous verified backup remains until the next update passes its health checks. Keep enough free space for staging, the installed files, and overlapping backup generations. The CLI launcher, service files, selected app, and backup directory must share a filesystem. Low space stops the update.

The updater removes its temporary installers and owned staging files after use. Recovery files remain when an operation fails. It does not clean Downloads or unrelated backups. T3 runtime caches are outside this backup retention policy.

See [recovery details](docs/recovery.md). To recover one device:

```sh
t3-update-preview --status
t3-update-preview --rollback work-server
```

Rollback refuses to replace data or settings that have changed, appeared, or been deleted since the recovery checkpoint. SQLite checks include uncheckpointed writes in its WAL file. `--force` does not permit loss of newer data. A schema migration can also block automatic rollback, even without new user messages. If recovery is blocked or interrupted, retain the journal and backup. Do not delete the operation directory or manually overwrite the database to make the error disappear.

If the original direct or relay route is down, rollback can use the pinned SSH identity. A working route to a different environment blocks recovery. A restored host with an unverified original route is reported separately.

## Files and removal

Configuration and checker state use the current user's platform directories. Linux honors the applicable XDG paths. `--help` shows the defaults; `--config` and `--state-dir` accept absolute overrides. Setup saves configuration with mode `0600` in a private `0700` directory. Keep import files private too.

To remove the updater, first run `--setup`, keep enrollment, and answer `no` for notices. This removes its user scheduler files. Then remove the executable from the location where you installed it. Keep per-device recovery data until you no longer need rollback. Removing this updater does not uninstall T3.

## Errors and privacy

Exit code `0` means the requested check or operation succeeded. A newer available release is not an error. Code `1` reports an operational failure, `2` reports invalid command arguments, and `3` reports a blocked or deferred operation. Read the device message; an error after activation can require recovery.

This project has no telemetry or hosted coordinator. Release lookup contacts GitHub. Device operations contact only selected enrolled routes. Credentials remain in your existing SSH setup. Configurations, import files, journals, and backups can contain private device details or T3 data. Do not attach them to public issues. Report the command, updater version, OS/CPU, and a redacted error instead. For a security report, use the repository's private vulnerability-report route when available; do not post a working secret or private backup.

## Development

```sh
make check
```

This runs Go race tests, Python host-worker tests, a build, formatting checks, and `go vet`. Fixtures cover release selection and hostile archives, device selection, check concurrency and retries, schedule files, and host update/recovery paths. Native jobs and desktop notices use fake runners during tests. CI runs on macOS and Linux. Cross-compilation does not prove live desktop, relay, or sleep/wake behavior. See [connection compatibility](docs/compatibility.md) for verified scope.

Released under the [MIT license](LICENSE).
