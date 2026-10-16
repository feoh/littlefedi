# littleMesh

littleMesh lets an instance federate from behind NAT or CGNAT without an inbound
port or its own public domain. A public relay, called a lighthouse, carries
encrypted connections between nodes. Each node's Ed25519 key is its identity.

The project does not currently provide public lighthouses. Whether to operate
them is still under consideration. You can self-host every component: instances,
lighthouses, HTTPS gateways and the optional directory.

A lighthouse alone connects mesh peers. Ordinary fediverse servers also need an
HTTPS gateway. An optional directory provides readable handles and signed relay
updates. Ask the service operator for relay addresses, node-ID pins, the namespace
and, if available, the directory URL.

## Run an instance

```sh
./littlefedi init
./littlefedi --config littlefedi.toml mesh id
./littlefedi --config littlefedi.toml
```

Choose **instance**, then **Join littleMesh**. Keep the HTTP listener on loopback
for local access. Use `host:7842@node-id` entries to pin the relays' identities.
The wizard creates the administrator, database and configuration.

A typical mesh configuration is:

```toml
[mesh]
enabled = true
suffix = "mesh.example.com"
lighthouses = ["relay.example.com:7842@<lighthouse-node-id>"]
exposure = "federation"
```

Replace the suffix and pin with values from your own relay or its operator.
Do not change the namespace after federation begins.
Without an explicit suffix, the first relay determines the domain; do not reorder
that list. A second pinned relay provides redundancy if both peers can reach it.

The instance's technical domain contains its node ID. `mesh id` prints the
identity and handles. Run it under the ordinary littleFedi service for your
[platform](install.md#start-the-service), with writable database and media paths.
No inbound firewall rule is needed on the node.

## Readable handles

Choose **Use a readable directory alias** during setup. You need an alias reserved
by the directory operator for the node ID printed by `init`.

```toml
[mesh]
enabled = true
suffix = "mesh.example.com"
alias = "red-house.mesh.example.com"
directory_url = "https://directory.mesh.example.com"
directory_state = "/var/lib/littlefedi/mesh-directory-client.db"
lighthouses = ["relay.example.com:7842@<lighthouse-node-id>"]
exposure = "federation"
```

Use your platform's data directory. The handle becomes
`@alice@red-house.mesh.example.com`; the actor URL stays bound to the node ID.
The gateway must use the same directory and suffix. Public directory records
require pinned public relays; private addresses and unpinned relays are not
published. After changing eligible relays, run:

```sh
./littlefedi --config littlefedi.toml mesh publish
```

Aliases cannot be transferred or recovered after loss of the node key.

## Run a lighthouse

Use a host reachable on TCP 7842. Give the relay its own service account and data
directory, separate from any instance on the same host.

```sh
littlefedi init --config /path/to/relay/lighthouse.toml
littlefedi --config /path/to/relay/lighthouse.toml lighthouse
```

Choose **lighthouse**. The wizard creates its identity and prints the node ID.
Publish the address with that ID, such as `relay.example.com:7842@<node-id>`.
Open the listener in the firewall. Relay TLS uses node keys and does not require
a public certificate.

For a private relay, set `[lighthouse] allowlist` to the permitted node IDs and
restart. For a public relay, keep bandwidth, circuit and connection limits
enabled; the configuration example lists them. An empty allowlist accepts any
node. Each relay needs its own database and identity.

`packaging/` includes `littlefedi-lighthouse` service definitions beside those
for the main instance. Check the account, working directory, configuration and
environment paths in the chosen file before enabling it.

## Add an HTTPS gateway

The gateway exposes mesh nodes to servers that do not speak littleMesh. It needs
wildcard DNS and a matching wildcard TLS certificate for the chosen namespace.
On the lighthouse, configure:

```toml
[mesh]
suffix = "mesh.example.com"

[lighthouse]
listen = ":7842"
gateway_listen = "127.0.0.1:8443"
gateway_mode = "federation"
```

Proxy HTTPS requests for `*.mesh.example.com` to `127.0.0.1:8443`, preserving the
host header. Examples are [Caddyfile-lighthouse](../packaging/Caddyfile-lighthouse)
and [nginx-lighthouse-gateway.conf](../packaging/nginx-lighthouse-gateway.conf).
Wildcard certificates need appropriate DNS validation or an existing certificate;
ordinary HTTP certificate validation is not sufficient.

For readable aliases, add `directory_url` and a writable `directory_state` path
to the gateway's `[mesh]` section too. Without them, alias requests fail even if
technical node-ID hosts work. Without a directory, nodes must register with the
relay used by the gateway. With multiple gateways, each needs a route to the node.

Keep `gateway_mode = "federation"` and node `exposure = "federation"` unless you
intend to expose the web interface and authentication endpoints publicly.

## Run a directory

Start from [littlemesh-directory.toml.example](../config/littlemesh-directory.toml.example).
Set a writable database path and put its loopback listener behind HTTPS, using
[Caddyfile-directory](../packaging/Caddyfile-directory). Keep registration closed.
Reserve an alias before the node first publishes:

```sh
littlefedi --config directory.toml directory reserve red-house.mesh.example.com <node-id>
littlefedi --config directory.toml directory
```

Service definitions are named `littlefedi-directory`. Run only one directory
process per database on local storage. `/healthz` checks its listener.

## Checks and backups

Use `littlefedi --config <config> mesh` for diagnostic command help. Check the
service logs for registration and quota errors. A gateway response of
`502 mesh node unreachable` means it has no working route to that node; check
registration, pins and relay reachability. Alias failures also require checking
the reservation, published record and gateway directory settings.

Back up the instance database: its `mesh_identity` table contains the private
key. Also back up `mesh.directory_state` and, for directory operators,
`directory.state_file`. Stop the owning process before copying these SQLite
files, or use SQLite's online backup command. See [backup and restore](backup-restore.md).

Relays cannot read mesh-to-mesh traffic, but they can observe connections and
deny service. An HTTPS gateway terminates TLS for ordinary fediverse callers:
its operator can read that traffic and alter unsigned responses. A directory
can mislead a caller's first alias lookup, but cannot impersonate a previously
known node ID. See [SECURITY.md](../SECURITY.md).
