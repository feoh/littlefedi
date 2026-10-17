# littleFedi - Feoh Fork!

## Fork maintainer note - This fork has nothing to do with Little Computer People other than being based on their excellent, excellent source code!
## Please find further IMPORTANT fork and migrations details [here](https://github.com/feoh/littlefedi/blob/main/FORK.md)

[littleFedi](https://littlefedi.org) is a small ActivityPub server written in Go.
It runs as a single binary, with SQLite, local media storage and a web interface
that works without JavaScript. Mastodon-compatible applications can connect
through its API. PostgreSQL, S3 storage and static blogs are optional builds.

littleFedi is beta software. Back up the database and local media before upgrading.

The public documentation is still incomplete. Extensive documentation already
exists, but it needs a full review before publication: recent changes have left
parts out of date, and some sections still contain information the developers
want to keep private. The project also has a fairly extensive test suite, which
is currently omitted from the public release. The reviewed documentation and
tests will be added progressively in future releases.

Public littleMesh lighthouses are not available at present. Whether to offer
them is still under consideration. All components can be self-hosted, including
lighthouses, HTTPS gateways and the optional directory; see the
[littleMesh guide](docs/littlemesh.md).

## Start from a binary

Download the binary for your operating system and CPU, along with `SHA256SUMS`.
The binary download directory also contains this README, the guides in `docs/`,
configuration examples in `config/` and service files in `packaging/`.

Check the download before renaming it. For example, on Linux amd64:

```sh
sha256sum -c --ignore-missing SHA256SUMS
cp littlefedi-linux-amd64 littlefedi
chmod 755 littlefedi
./littlefedi init
./littlefedi --config littlefedi.toml
```

Use the filename for your system. On macOS and BSD, replace `sha256sum` with
`shasum -a 256`. On Windows, keep the `.exe` extension and run `littlefedi.exe`;
there is no `chmod` step. Use PowerShell's `Get-FileHash -Algorithm SHA256` to
compare the download with its entry in `SHA256SUMS`.

Default release binaries use SQLite and store media on local disk. Optional
variants have `-postgres`, `-s3` or `-postgres-s3` in the filename. See
[database backends](docs/install.md#database-backends).

Choose **instance** in the wizard. It creates the configuration and administrator
account. Open `http://localhost:8080` to sign in. For a permanent installation,
follow the [installation guide](docs/install.md).

## Build from source

You need Go 1.26.6 or newer and GNU Make (`gmake` on BSD systems). Run these
commands from the extracted source directory:

```sh
make build
./littlefedi init
./littlefedi --config littlefedi.toml
```

Choose **instance** in the setup wizard. It writes the configuration and creates
the administrator account. With the default listen address, open
`http://localhost:8080` to sign in.

For federation over the Internet, choose the instance's permanent domain during
setup and serve it over HTTPS through a reverse proxy. Configure `trusted_proxies`
with the proxy's address. Examples for Caddy and nginx are in `packaging/`.
The [installation guide](docs/install.md)
covers service accounts, TLS, first startup and upgrades.

## Configuration

The [configuration example](config/littlefedi.toml.example) lists the available
settings. [littlefedi.production.toml](config/littlefedi.production.toml) is a
starting point for a single-host deployment; change its domains and paths before
starting the service.

```toml
local_domain = "example.com"
web_domain = "social.example.com"
```

`local_domain` forms account handles, such as `@alice@example.com`. `web_domain`
serves the web interface, API, media and ActivityPub objects. If they differ,
the handle domain must redirect WebFinger requests to the web domain.

On small devices, start with `low_power = true`. It reduces concurrency, cache
size and image-processing limits. The configuration example also covers blogs,
media limits and optional PostgreSQL and S3 backends.

## Build variants

| Command | Output | Features |
| --- | --- | --- |
| `make build` | `littlefedi` | SQLite, local media |
| `make build-postgres` | `littlefedi-postgres` | PostgreSQL, local media |
| `make build-s3` | `littlefedi-s3` | SQLite, local or S3 media |
| `make build-postgres-s3` | `littlefedi-postgres-s3` | PostgreSQL, local or S3 media |
| `make build-blog` | `littlefedi-blog` | SQLite, local media, static blogs |

Build tags select optional features at compile time. For example,
`make build-s3 TAGS=blog` includes both S3 and blogs. `make build-all` builds the
five variants above. These builds disable CGO and require no separate frontend
build step.

`make release` builds the common Linux, BSD, macOS, illumos and Windows targets.
`make allreleases` takes the full platform list from the installed Go toolchain,
excluding iOS, WebAssembly and Android targets that require CGO. Both commands
include blogs, build ARMv6 and ARMv7 variants for 32-bit ARM, and write
platform-named binaries and licence notices to `dist/`.

Many of these OS and CPU combinations have not been tested on the target system.
A successful build does not guarantee that the server will run correctly there.
Untested targets are provided for experimentation. Check startup, federation,
backup and restore on your host before relying on them.

Use `TAGS=s3` or `TAGS="postgres s3"` for other backends, with a separate output
directory to keep the variants:

```sh
make allreleases TAGS="postgres s3" DIST_DIR=dist/postgres-s3
```

To cross-compile, set the Go target variables:

```sh
GOOS=linux GOARCH=arm GOARM=6 make build
GOOS=freebsd GOARCH=amd64 make build
```

Both commands write `littlefedi`; move or rename the output before building
another target. SQLite support depends on the target architecture; see the
[installation notes](docs/install.md#database-backends).

## Administration

```sh
./littlefedi --config littlefedi.toml admin create-user --username alice --admin
./littlefedi --config littlefedi.toml admin list-users
./littlefedi --config littlefedi.toml admin reset-password --username alice
./littlefedi --config littlefedi.toml backup now --media
```

Run `./littlefedi help` for the command list. Use the binary matching your
database and media backend, and run administrative commands as the service user.

`/health` checks that the process is alive; `/readyz` also checks storage.
Keep the application listener behind the reverse proxy. Follow the
[backup and restore guide](docs/backup-restore.md) before restoring data or
rolling back an upgrade.

## Documentation

- [Installation and upgrades](docs/install.md)
- [Backup and restore](docs/backup-restore.md)
- [littleMesh](docs/littlemesh.md)
- [Security reports](SECURITY.md)

Trends, follow suggestions and endorsements return empty results. Local custom
emoji uploads are not supported. Media URLs are public bearer URLs: anyone who
has a URL can access that file, including media attached to a private post.

## Licence

littleFedi is a project by the Little Computer People, maintained by Stefano
Marinelli. It is distributed under the [MIT licence](LICENSE).
See [NOTICE.md](NOTICE.md) for project attribution and
[THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md) for dependency and asset notices.
From the source directory, run `make licenses` after changing dependencies or
bundled assets to update them.

Copyright © 2026 Stefano Marinelli.
