# Installation

The usual setup is one littleFedi process, SQLite and local media, behind a TLS
reverse proxy. Choose the instance's permanent domain before creating accounts.
For a host without a public domain or inbound ports, use [littleMesh](littlemesh.md).

Many of the OS and CPU combinations in the downloads have not been tested on
the target system. Treat those builds as experimental. Before using one for a
live instance, check startup, federation, backup and restore on that host.

## Files and service account

Build from the [README](../README.md), or download a binary for your system.
The binary download directory includes the guides, configuration examples and
service files used below. Install the chosen binary as `/usr/local/bin/littlefedi`,
owned by root, mode `0755`.

Check the downloads before installing them. If you downloaded the source
archive, run this from its download directory:

```sh
shasum -a 256 -c littlefedi-26.10.13-source.tar.gz.sha256
```

The binary download directory includes `SHA256SUMS`, covering all binaries and
their documentation, configuration examples, service files and licences.
Run `shasum -a 256 -c SHA256SUMS` from that directory.
On Linux, use `sha256sum -c` instead. If you downloaded only one binary, check
its entry with `shasum -a 256 -c --ignore-missing SHA256SUMS`.

The supplied service files use these paths:

| System | Account | Data directory | Configuration directory |
| --- | --- | --- | --- |
| Linux, Alpine | `littlefedi` | `/var/lib/littlefedi` | `/etc/littlefedi` |
| FreeBSD, NetBSD | `littlefedi` | `/var/db/littlefedi` | `/usr/local/etc/littlefedi` |
| OpenBSD | `_littlefedi` | `/var/littlefedi` | `/etc/littlefedi` |
| illumos | `littlefedi` | `/var/littlefedi` | `/etc/littlefedi` |

Create a dedicated account and group using your system's account tools. It must
have no interactive password. As root, create its data directory with mode
`0700`, owned by that account. Create the configuration directory with mode
`0755`, owned by root. The files in it are `0640`, owned by root and the service
group, so a lighthouse or directory on the same host can use the same directory.

For example, on Linux with systemd:

```sh
sudo useradd --system --user-group --home /var/lib/littlefedi --shell /usr/sbin/nologin littlefedi
sudo install -d -o littlefedi -g littlefedi -m 0700 /var/lib/littlefedi
sudo install -d -o root -g root -m 0755 /etc/littlefedi
sudo install -o root -g root -m 0755 littlefedi /usr/local/bin/littlefedi
sudo -u littlefedi /usr/local/bin/littlefedi init --config /var/lib/littlefedi/littlefedi.toml
sudo install -o root -g littlefedi -m 0640 /var/lib/littlefedi/littlefedi.toml /etc/littlefedi/littlefedi.toml
```

On other systems, run `littlefedi init --config <data-directory>/littlefedi.toml`
as the service account, then copy the configuration to the path in the table,
owned by root and the service group, mode `0640`. The wizard creates the database
beside the generated configuration and asks for the first administrator account.

## Configuration

Choose **instance** in the wizard and use `127.0.0.1:8080` as the HTTP listener
when the reverse proxy is on the same host. Set `trusted_proxies` to the proxy's
addresses only. Keep registration closed while setting up the instance.

In the installed configuration, set absolute paths for `[db] dsn`, `[media] path`
and `[backup] path`, using your data directory. If blogs are enabled, also set
`[blog] output_dir` there. Create the media and backup directories as the service
account. The complete reference is [littlefedi.toml.example](../config/littlefedi.toml.example).

Create `littlefedi.env` in the configuration directory, owned by root and the
service group, mode `0640`. The systemd unit requires this file even if it is
empty. Use shell-compatible `KEY=value` assignments, quoting special characters.
Set a stable `LITTLEFEDI_MEDIA_PROXY_SECRET` using `openssl rand -hex 32`.
For MFA, set `LITTLEFEDI_AUTH_MFA_ENCRYPTION_KEY` using `openssl rand -base64 32`.
Keep these values across restarts and back them up separately from the database.

If using SMTP, set the connection details in the configuration and the password
in `LITTLEFEDI_SMTP_PASSWORD`. Check mail delivery before requiring verified email.
Enrol the administrator in MFA before enabling `auth.require_admin_mfa`.

Account signing keys can be encrypted at rest. Set
`LITTLEFEDI_FEDERATION_KEY_ENCRYPTION_KEY` to a separate base64-encoded 32-byte key,
stop the service, load its environment and run `littlefedi --config <config>
admin keys encrypt`. Keep the key: without it, a restored database cannot sign
for its accounts. Older backups may still contain unencrypted keys.

## HTTPS

Point the public domain's DNS records at the reverse proxy. Use
[Caddyfile](../packaging/Caddyfile) or [nginx-littlefedi.conf](../packaging/nginx-littlefedi.conf),
replacing the example domain. The nginx example also needs
[littlefedi-proxy-headers.conf](../packaging/littlefedi-proxy-headers.conf) at the
path named in its `include`, plus a valid certificate and automatic renewal.
Caddy obtains certificates for ordinary public domains automatically.

Expose the proxy on TCP 80 and 443. Keep port 8080, the database, metrics and
profiler private. Firewall examples are included under `packaging/<system>/`;
adapt them to your host before applying them, preserving administrative access.

## Start the service

Install the file below as root. Shell service scripts need executable permissions
(`0755`); systemd units and SMF manifests use `0644`.

| System | File | Install as | Enable and start |
| --- | --- | --- | --- |
| Linux | `packaging/linux/littlefedi.service` | `/etc/systemd/system/littlefedi.service` | `systemctl daemon-reload`, then `systemctl enable --now littlefedi` |
| Alpine | `packaging/alpine/openrc/littlefedi` | `/etc/init.d/littlefedi` | `rc-update add littlefedi default`, then `rc-service littlefedi start` |
| FreeBSD | `packaging/freebsd/rc.d/littlefedi` | `/usr/local/etc/rc.d/littlefedi` | `sysrc littlefedi_enable=YES`, then `service littlefedi start` |
| NetBSD | `packaging/netbsd/rc.d/littlefedi` | `/etc/rc.d/littlefedi` | Set `littlefedi=YES` in `/etc/rc.conf`, then `/etc/rc.d/littlefedi start` |
| OpenBSD | `packaging/openbsd/rc.d/littlefedi` | `/etc/rc.d/littlefedi` | `rcctl enable littlefedi`, then `rcctl start littlefedi` |
| illumos | `packaging/illumos/smf/littlefedi.xml` | `/lib/svc/manifest/site/littlefedi.xml` | `svccfg import /lib/svc/manifest/site/littlefedi.xml`, then `svcadm enable site/littlefedi` |

On OpenBSD, the script requires a `littlefedi` login class in `/etc/login.conf`;
a minimal entry is `littlefedi:tc=daemon:`. On illumos, create the project used by
the manifest with `projadd -U littlefedi littlefedi`. Increase descriptor limits
if needed, keeping them below the host's limits. NetBSD and OpenBSD's supplied
scripts do not restart a crashed process automatically.

Start the reverse proxy too, then check:

```sh
curl --fail http://127.0.0.1:8080/health
curl --fail http://127.0.0.1:8080/readyz
curl --fail https://social.example.com/.well-known/nodeinfo
```

Sign in at `/auth/sign_in`, publish a post with an image and check a follow in
both directions with another instance. Logs go to the systemd journal on Linux,
`/var/log/littlefedi/littlefedi.log` on Alpine, syslog on BSD, and the SMF service
log on illumos. Check free disk space, backup age and `/readyz` regularly.

## Upgrades

Stop littleFedi, [take a backup](backup-restore.md) and copy it off the host.
Install the new binary, start the service and check `/readyz` and the logs.
Database migrations run at startup. To roll back, stop the service and restore
the pre-upgrade backup before starting the old binary.

## Database backends

The default build includes SQLite. The driver is selected when compiling:

| System | Architectures using modernc SQLite |
| --- | --- |
| macOS, Windows | All release architectures |
| FreeBSD | 386, amd64, arm, arm64 |
| Linux | 386, amd64, arm, arm64, loong64, ppc64le, riscv64, s390x |
| NetBSD | amd64 |
| OpenBSD | amd64, arm64 |

Other targets use the portable ncruces driver, including illumos, DragonFly BSD,
NetBSD arm/arm64/386 and Linux MIPS/ppc64. This driver uses at most two database
connections, or one on illumos and Solaris. Backups require a `sqlite3` command
in `PATH`; set `LITTLEFEDI_SQLITE3` to choose a specific executable. The Go memory
limit is calculated at startup unless you set `GOMEMLIMIT`.

Some targets also have known driver limitations. AIX, Plan 9 and Solaris builds
lack SQLite file locking and shared-memory WAL support with the release's build
tags, so the default SQLite setup can fail during initialization. These binaries
remain available for experimentation; they have not been tested on those systems.

For PostgreSQL, use a `littlefedi-postgres-*` binary when available, or build with
`make build-postgres`. Provide a PostgreSQL DSN via `LITTLEFEDI_DB_DSN`.
Backup and restore need `pg_dump` and `psql`. For S3, use a `littlefedi-s3-*` or
`littlefedi-postgres-s3-*` binary, or build with the `s3` tag. Configure
`[media.s3]`; account exports also need a separate, private bucket. Credentials
can be supplied through the environment variables listed in the configuration
example. Back up S3 with the provider's tools.

When running several processes, use PostgreSQL, shared S3 storage and
`streaming.backend = "postgres"`. Enable `background_maintenance` on exactly one
process, disable local remote-media caching and blogs, and apply shared rate
limits at the proxy. Start one process first after upgrades so migrations finish
before starting the others.
