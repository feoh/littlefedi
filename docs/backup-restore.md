# Backup and restore

Keep an encrypted copy on another machine and test a restore before relying on
it. Backups contain account identities and credentials; keep access restricted.

## Create a backup

Run as the service account with the same environment file loaded as the service:

```sh
littlefedi --config <config> backup now --media
```

The command prints the backup directory. It contains a database snapshot,
configuration, manifest and local owned media. Remote media caches are excluded.
Copy the complete directory off the host. Keep the local backup directory at
mode `0700`. Encryption keys from the environment need a separate secure backup.

To schedule backups, configure `[backup] enabled`, `interval`, `path`, `retain`
and `include_media` in [littlefedi.toml.example](../config/littlefedi.toml.example).
Check both the age of the newest backup and the off-host copy.

Builds using the portable SQLite driver need the `sqlite3` command for backups;
see the [driver table](install.md#database-backends) for your platform.
Set `LITTLEFEDI_SQLITE3` to choose a specific executable.
PostgreSQL backups need `pg_dump` and restores need `psql`; use client
tools compatible with your server and supply credentials through the environment
or a protected password file.

The built-in backup does not copy S3 media. Use `--no-media` for its database
backup and back up the media and private export buckets separately with your
provider's tools. Verify restoration of the objects as well as the database.

## Restore

1. Stop littleFedi. Use an isolated host for a rehearsal so the restored instance
   cannot send mail or federate under the live instance's identity.
2. Install the matching release. Recover the saved configuration and environment
   keys. Keep the original domains, and set the database and media paths to the
   intended restore locations. The restore command does not install the saved
   configuration for you.
3. Prepare an empty data directory owned by the service account, then run:

   ```sh
   littlefedi --config <config> backup restore /path/to/backup
   ```

4. Start the service and check `/health`, `/readyz`, account access, posts and
   local media. Check MFA and encrypted signing keys with the recovered keys.
5. For a real recovery, reconnect the instance only after these checks and after
   ensuring the previous copy is stopped. Keep a rehearsal isolated.

To roll back an upgrade, restore the pre-upgrade backup and run the old binary.
Do not run an older binary against a database already migrated by a newer release.
See [installation and upgrades](install.md#upgrades).

## littleMesh

The instance database includes its mesh private key in `mesh_identity`. Losing
that key loses the node's identity. Also save `mesh.directory_state` for readable
aliases, and `directory.state_file` if you operate a directory.

These additional databases use SQLite WAL mode. Stop their owning process before
copying the database, or use SQLite's online backup command. Never copy only the
main file while uncheckpointed changes remain in a `-wal` file. Restore these
files before reconnecting the node or directory, and confirm the node ID with
`littlefedi --config <config> mesh id`.
