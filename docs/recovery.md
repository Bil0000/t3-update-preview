# Recovery

Run the updater from a terminal outside T3. Start with:

~~~sh
t3-update-preview --status
~~~

Status checks each selected device, even if another device is unavailable. Saved recovery status does not require a working T3 URL. Remote recovery still needs SSH access.

Recover one enrolled device with:

~~~sh
t3-update-preview --rollback DEVICE_ID
~~~

Use the device ID shown during setup. Exclusions also apply to recovery. Force mode permits interruption of that device's T3 work. It does not bypass identity, backup integrity, or newer-data checks.

The tool stores its private journal and backup in the update-preview folder under the selected T3 base directory. After a successful update, it keeps one rollback generation. It removes the previous generation only after the replacement passes health checks. Failed operations can keep additional recovery evidence.

Rollback checks saved database and file state before replacement. If new work or settings make the checkpoint unsafe, it refuses to overwrite them. Keep the recovery folder and inspect the reported paths. Do not delete it to clear an error or copy a live SQLite file over an active database.

Updates apply to remote devices first, then the local device. A failure stops later activations. Devices that already passed stay at their new version. Check status before retrying; the result can be a mix of versions.

A host can recover while its direct or relay URL remains unavailable. The command reports that distinction and returns an error until it can verify the original route. A public endpoint check does not prove that the desktop's authenticated connection has reconnected.

Installer files are temporary. A crash or power loss can leave files behind until later cleanup or recovery. Keep files that the tool reports as recovery evidence.
