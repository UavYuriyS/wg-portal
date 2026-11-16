WireGuard Portal can run as a high availability (HA) cluster: two or three WireGuard **nodes** at different
sites serve the users, and a separate **orchestrator** runs the web UI and the control plane. Clients connect
to a DNS name (for example `vpn.example.com`) that always points at the **active** node. All other nodes are
hot standbys. Each party keeps a full copy of the database, replicated with MariaDB Galera.

The design and its rationale are documented in [docs/design/ha.md](https://github.com/h44z/wg-portal/blob/master/docs/design/ha.md).

```
admin ──https──> portal.example.com ──> ORCHESTRATOR  (UI, API, probes, failover, DNS updates)
                                              │ Galera (mutual TLS)
              ┌───────────────────────────────┼───────────────────────────────┐
users ─udp─> vpn.example.com ──> NODE A (active)        NODE B (standby)        NODE C (standby)
```

## How it works

- **Roles.** The same binary runs in one of three roles (`ha.role`):
    - `standalone`: today's single instance, the default.
    - `orchestrator`: web UI, peer expiry and LDAP sync, health probes, failover and DNS updates. It never
      touches WireGuard.
    - `node`: applies the replicated database to its kernel every `reconcile_interval` and has no public UI.
- **Every node is hot.** All nodes run the interface with the same key, port and peers, so a promotion only
  changes the DNS record and which node writes statistics.
- **Health probe.** The orchestrator connects to every node's *public IP* with an in-process WireGuard client
  (the probe peer it creates per server interface) and requests `ha.probe_url` (ipify) through the tunnel.
  The node is healthy when the observed egress IP equals the **node's own public IP**.
    - The probe connects to the port of the interface's default peer endpoint (what users connect to), or to
      the listen port when the endpoint has no port. A port forward in front of WireGuard is therefore probed
      through, like a user would connect.
    - Place the orchestrator in the same network region as your users, so a node that is blacklisted for them
      also fails the probe.
    - The node must NAT the tunnel traffic (for example a `PostUp` masquerade rule).
    - The probe peer never uses an [exit node](routing-policy.md): an exit host only has a tunnel to the
      active node, so probes through standbys would never pass and failover would deadlock.
- **Exit check.** When an exit node is active, the orchestrator also probes through the *active* node with a
  second probe peer (`ha-probe-exit ...`) that is routed like a normal client, and compares the egress IP with
  the active exit candidate's egress IP. The result is shown on the *High Availability* page
  (`ExitCheck` in `GET /ha/status`). It is informational only and never triggers a failover. Only the first
  server interface is checked.
- **Failover.**
    - When the active node fails `fail_threshold` probes in a row, the orchestrator promotes the healthy
      standby with the highest priority and rewrites the reg.ru A record.
    - A healthy standby has a passing probe and a fresh heartbeat.
    - Fail-back is never automatic, and `min_failover_interval` prevents flapping.
    - Admins can promote any node in the UI (*High Availability* in the user menu) or through the API.
- **Orchestrator down.** Nodes keep serving users from their local database copy, and the active node keeps
  writing statistics. Only admin work, peer expiry, LDAP sync and failover pause.

## Requirements

- MariaDB 10.6+ (the compose files use 11.4) on every party. wg-portal must use `database.type: mysql`.
- The WireGuard listen ports of all server interfaces open on the nodes, for users and the health probes.
- Galera ports open between all parties: `4567/tcp+udp`, `4568/tcp` and `4444/tcp`. The traffic is
  authenticated and encrypted with certificates from the cluster CA.
- A low TTL (30–60s) on the VPN record. The zone must be hosted on reg.ru DNS servers, and API access must be
  allowed from the orchestrator's IP in the reg.ru account settings.
- The orchestrator plus at least two nodes, so Galera keeps quorum (3 members) when one party fails.

## Setup

### 1. Orchestrator

Copy `deploy/ha/docker-compose.ha-orchestrator.yml` and `deploy/ha/initdb/` to the orchestrator host and
create a `.env` file:

```
PUBLIC_IP=198.51.100.10               # Galera address of the orchestrator
DB_PASSWORD=...
DB_ROOT_PASSWORD=...
SST_PASSWORD=...
ENCRYPTION_PASSPHRASE=...             # encrypts keys in the database, handed to the nodes when they join
EXTERNAL_URL=https://portal.example.com
REGRU_USERNAME=...
REGRU_PASSWORD=...
REGRU_ZONE=example.com
REGRU_SUBDOMAIN=vpn
UI_BIND=127.0.0.1:8888                # optional, this is the default
```

Then run `docker compose -f docker-compose.ha-orchestrator.yml up -d`. On its first start, `ha-init` creates
the cluster CA in `./data/ha` and MariaDB bootstraps a new cluster. Put the UI behind HTTPS
(see [Reverse Proxy](reverse-proxy.md)): nodes join through it.

The orchestrator does not use host networking. Only the Galera ports are published on all addresses. The UI
is published on `UI_BIND`, by default on the loopback address only. If the reverse proxy runs in a container
on the same host, either set `UI_BIND` to the Docker bridge address (for example `172.17.0.1:8888`), or attach
the `wg-portal` service to the proxy's network and proxy to `wg-portal:8888`. Ports that Docker publishes
bypass host firewalls such as ufw; to restrict the Galera ports to the node addresses, use the `DOCKER-USER`
chain.

!!! warning
    Back up `./data/ha/ca.pem` and `./data/ha/ca-key.pem`. A copy is also stored, encrypted, in the database
    (see *Replacing the orchestrator*).

Create the WireGuard server interface in the UI as usual:

- Set the peer default endpoint to the VPN DNS name, for example `vpn.example.com:51820`.
- Add NAT and forwarding rules to `PostUp`/`PostDown`. The masquerade rule is required: on the nodes, the
  interface lives in a container network namespace, and Docker only NATs traffic from the container's own
  address. The `FORWARD` rules keep the same rules working on hosts whose `FORWARD` policy is `DROP`. Without
  them, handshakes work, but no traffic passes and every health probe fails:

  ```
  PostUp:   iptables -I FORWARD -i %i -j ACCEPT; iptables -I FORWARD -o %i -j ACCEPT; iptables -t nat -A POSTROUTING -s 10.11.12.0/24 ! -d 10.11.12.0/24 -j MASQUERADE
  PostDown: iptables -D FORWARD -i %i -j ACCEPT; iptables -D FORWARD -o %i -j ACCEPT; iptables -t nat -D POSTROUTING -s 10.11.12.0/24 ! -d 10.11.12.0/24 -j MASQUERADE
  ```

The orchestrator creates the health probe peers (`ha-probe ...` and `ha-probe-exit ...`) on its own. Do not
delete them.

### 2. Nodes

In the UI, open *High Availability* and click **Create join token**. Alternatively, an admin can request one
through the API: `POST /api/v1/ha/join-tokens` with basic auth.

On each node, copy `deploy/ha/docker-compose.ha-node.yml` and run:

```
WG_PORTAL_HA_ORCHESTRATOR_URL=https://portal.example.com \
WG_PORTAL_HA_JOIN_TOKEN=<token> \
WG_PORTAL_HA_PRIORITY=10 \
docker compose -f docker-compose.ha-node.yml up -d
```

What happens on the node:

1. `ha-init` registers the node at `POST /api/v1/ha/register`. The node ID is the hostname and the public IP
   is detected via ipify; override them with `WG_PORTAL_HA_NODE_ID` and `WG_PORTAL_HA_PUBLIC_IP`.
2. It receives a node certificate, the cluster members and the database credentials, and writes them to
   `./data/ha`.
3. MariaDB joins the cluster and copies the database (SST).
4. wg-portal brings up all interfaces and peers.

The token is only needed for this first start. Join one node at a time.

Nodes do not use host networking either. A small `net` service owns the network namespace that `mariadb` and
`wg-portal` share, so the WireGuard interfaces, routes and firewall rules live there and survive restarts and
upgrades of both services. What this means:

- **Ports.** `net` publishes the Galera ports and the WireGuard ports in `WG_PORTS` (`.env`, default `51820`).
  List every listen port of the cluster's server interfaces as one range, for example `WG_PORTS=51820-51829`.
  A node without the port of an interface fails its health probes. Changing `WG_PORTS` recreates the
  containers, so the node's tunnels drop for a moment: change standbys first. Ports that Docker publishes
  bypass host firewalls such as ufw; use the `DOCKER-USER` chain to restrict them.
- **Restarts.** Restart the node with `docker compose restart` or `up -d`, never the `net` container alone with
  `docker restart`: the other containers would keep running in its old, unreachable namespace.
- **Inspecting.** `wg show`, `ip rule` and `iptables` on the host do not show the node's state. Run them in the
  container, for example `docker compose -f docker-compose.ha-node.yml exec wg-portal wg show`.
- **Egress.** Tunnel traffic leaves with the host's address, like before. Peers are not reachable from the
  host's other networks, because these networks have no route into the container. IPv6 egress needs
  `enable_ipv6` on the compose network.

!!! note "Nodes joined with host networking"
    A node that joined with an older compose file keeps its config and database. Stop it with
    `docker compose down`, delete its old interfaces on the host (`ip link del wg0`, or reboot the host), then
    start it with the new compose file. The old interface would block the WireGuard port otherwise. Upgrade
    the standbys first and promote one of them before you switch the active node.

The first healthy node becomes active automatically. The orchestrator then points the DNS record at it.

### 3. Clients

Clients have to resolve the DNS name again after a failover. Desktop WireGuard clients resolve it once, when
the tunnel starts.

- **Linux.** Install `deploy/ha/clients/wg-reresolve.sh` as `/usr/local/bin/wg-reresolve.sh`. Copy the
  `wg-reresolve@.service` and `wg-reresolve@.timer` files to `/etc/systemd/system`, then run
  `systemctl enable --now wg-reresolve@wg0.timer`. Every 30s, the script re-resolves the endpoint of peers
  whose last handshake is older than 135s.
- **Windows.** As administrator, run
  `deploy\ha\clients\Install-WgReresolve.ps1 -Tunnel <tunnel name> -Endpoint vpn.example.com:51820`.
  This registers a scheduled task that does the same every minute, running as SYSTEM.
- **Mobile.** Toggling the tunnel off and on resolves the name again.

Expected client downtime after a node fails:

> probe interval × fail threshold + DNS TTL + re-resolve interval + handshake

That is about 2–4 minutes with the defaults. Clients that cache DNS longer than the TTL take longer.

## Migrating an existing installation

An existing standalone installation (SQLite) is imported into the cluster database. Server keys and peers
stay the same, so clients keep their configs. They only need to reach the server through the VPN DNS name.

1. **Check the client endpoint.** If the interface's peer default endpoint is an IP address, change it to the
   VPN DNS name and click *Apply peer defaults*, then hand out new client configs. Existing peers keep the
   endpoint they were created with. If it is already the DNS name, clients need no change.
2. **Lower the TTL.** A day before the migration, set the TTL of the VPN record to 60s.
3. **Prepare the orchestrator.** Set up the orchestrator as above, but **do not start wg-portal yet**.
    - Use the `encryption_passphrase` of the old installation. If it never had one, keep
      `ENCRYPTION_PASSPHRASE=` empty, or set a new passphrase to encrypt the keys from now on: the import
      encrypts all keys and API tokens that the old installation stored in plain text.
    - Start only the database: `docker compose -f docker-compose.ha-orchestrator.yml up -d mariadb`.
4. **Stop the old wg-portal and import its database.** WireGuard keeps running in the kernel, so users stay
   connected while wg-portal is stopped.
    - Copy its `data/sqlite.db` to the orchestrator's `./data/` directory.
    - Run `docker compose -f docker-compose.ha-orchestrator.yml run --rm wg-portal -ha-import /app/data/sqlite.db`.
    - A temporary copy of the file is upgraded to the current schema first, so the original stays
      untouched and older v2 versions can be imported.
    - The import refuses to write into a database that already contains data. Run it before the
      orchestrator's first start.
5. **Start the orchestrator:** `docker compose -f docker-compose.ha-orchestrator.yml up -d`. Users, including
   the admin account and its password, are the ones from the old installation.
   Once the UI shows the interfaces and peers, **delete `./data/sqlite.db`** from the orchestrator. The import
   never changes the source file: if the old installation had no passphrase, it holds every private key in
   plain text, and `./data/` is the directory you back up for the cluster CA.
6. **Join the nodes.** They come up with the old server key, port and peers. The first healthy node becomes
   active and the VPN DNS record is pointed at it.
7. **Reuse the old server, if you want.** Stop its old wg-portal and move `sqlite.db` off the server into a
   protected backup, or delete it (it may hold all keys in plain text). Then join it as a node with the
   highest priority. Delete its old interface before the first start (`ip link del wg0`): the node brings
   up its own copy in the container, and the old one would block the port.

!!! note
    If the portal URL changes, passkeys (WebAuthn) registered under the old domain stop working. Users can
    register them again.

`-ha-import` only reads SQLite files. For an installation on MySQL, Postgres or MSSQL, move the data with
that database's own tools.

## Operations

| Task | How |
|------|-----|
| Promote a node | UI → High Availability → Promote, or `POST /api/v1/ha/promote/{nodeId}` |
| Route clients through an exit node | UI → Routing, see [Exit nodes and routing policy](routing-policy.md). The former *exit node IP* setting was removed; each exit candidate has its own egress IP. |
| Remove a standby | UI → Remove (promote another node before removing the active one) |
| Upgrade | One party at a time: orchestrator first, then the nodes. Never restart all parties at once. |
| Full cluster restart | Standard Galera procedure: start the member with the most recent state first, with `safe_to_bootstrap: 1` in `grastate.dat` |

### Replacing the orchestrator

1. On any node, export the CA from its database copy:
   `docker compose -f docker-compose.ha-node.yml run --rm wg-portal -ha-export-ca /app/data/ca-export`.
2. On the new orchestrator host, copy `ca.pem` and `ca-key.pem` into `./data/ha/` and write the node IPs into
   `./data/ha/members` (one per line), so it joins the existing cluster instead of creating a new one.
3. Start the orchestrator compose with the same `.env`. Point `portal.example.com` at the new host and allow
   its IP in the reg.ru API settings.

## Configuration reference (`ha` section)

| Key | Default | Description |
|-----|---------|-------------|
| `role` | `standalone` | `standalone`, `orchestrator` or `node` |
| `node_id` | hostname | Unique name of this party |
| `public_ip` | | Galera address; for nodes also the address users and probes connect to (required on the orchestrator) |
| `priority` | `0` | Higher wins when a standby is promoted |
| `data_dir` | `data/ha` | CA, certificates, `galera.cnf`, generated node config |
| `heartbeat_interval` | `10s` | Node heartbeat |
| `heartbeat_timeout` | `60s` | A standby with an older heartbeat is not promoted |
| `reconcile_interval` | `30s` | How often nodes apply database changes to the kernel |
| `probe_interval` / `probe_timeout` | `30s` / `15s` | Health probes |
| `probe_url` | `https://api.ipify.org` | Returns the caller IP as plain text |
| `fail_threshold` | `3` | Failed probes in a row before failover |
| `min_failover_interval` | `5m` | Flap guard |
| `auto_failover` | `true` | `false`: only manual promotion |
| `on_promote` | | Shell command run on the orchestrator after a promotion (`NODE_ID`, `NODE_IP`, `PREVIOUS_NODE_ID`) |
| `orchestrator_url`, `join_token` | | Node only, used by `-ha-init` |
| `galera.cluster_name`, `galera.sst_user`, `galera.sst_password` | `wg-portal`, `mariabackup`, | Orchestrator; handed to nodes on join |
| `dns.regru.api_url`, `username`, `password`, `zone`, `subdomain` | | reg.ru API v2 credentials and the record to update |

All keys can be set with environment variables named `WG_PORTAL_HA_<KEY>`, for example
`WG_PORTAL_HA_DNS_REGRU_ZONE`.
