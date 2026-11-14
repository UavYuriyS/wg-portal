# High availability design: orchestrator + replicated hot-standby WireGuard nodes

Status: implemented 2026-11-14 (see "Implementation notes" at the end for where the code differs from the plan). Keep this file updated when the implementation diverges.

## Context
wg-portal assumes it is the only instance:
- There is no election and no locking.
- The event bus runs in-process (`cmd/wg-portal/main.go:75`).
- The DB is pushed to the kernel only once, at startup (`RestoreInterfaceState`, `internal/app/wireguard/wireguard_interfaces.go:204`).

Goal: 2–3 WireGuard nodes at different sites/clouds, local kernel backend. Clients connect to `vpn.<domain>`, a low-TTL A record hosted at **reg.ru**.

The **orchestrator** is a separate wg-portal instance that holds the UI and the control plane. If it dies, only admin work stops; the nodes keep serving users. Every party holds a full DB replica, so a replacement orchestrator can be started and the portal DNS name moved to it.

The orchestrator health-checks the nodes. The failure we care about is a node whose IP is blacklisted: it is alive but unusable. When the active node fails, the orchestrator promotes a standby and rewrites the reg.ru record, automatically or on an admin click.

## Architecture
```
               admin ──https──> portal.<domain> ──> ORCHESTRATOR (wg-portal, role=orchestrator)
                                                     UI/API, expiry, LDAP, failover loop, probes, reg.ru updater
                                                     Galera member
                                                          │  Galera replication (mutual TLS, cluster CA)
                         ┌────────────────────────────────┼────────────────────────────────┐
users ─udp─> vpn.<domain> (A = active node IP)            │                                │
                 NODE A (role=node, ACTIVE)          NODE B (role=node, standby)      NODE C (standby)
                 wg0 up, reconcile loop, stats       wg0 up (hot), reconcile loop      same
                 Galera member                       Galera member                     Galera member
```
- **One binary, three roles.** `ha.role: standalone | orchestrator | node`. The default is `standalone`, which is today's behaviour.
- **Galera members are the orchestrator plus every node.**
  - A dyad (2 nodes + orchestrator) has 3 members, which is quorum without `garbd`.
  - Galera traffic uses mutual TLS with a cluster CA that the orchestrator issues (see Node registration).
- **Every node is hot.** Interface key, listen port and peers come from the replicated DB, so a promotion only changes DNS and which node writes statistics. A client with stale DNS that lands on a standby still works.
- **Orchestrator down:** nodes keep running from their local replicas. Stats keep flowing on the active node. Only peer CRUD, expiry, LDAP sync and failover stop. Recovery:
  1. Start a new orchestrator.
  2. Let it join Galera; SST copies the DB.
  3. Point `portal.<domain>` at it.
  4. Add its IP to the reg.ru API whitelist.
- **Orchestrator placement:** it should sit in the same network region as your users. Its probes are meant to show what users see; probing from outside the blocked region would report a blacklisted node as healthy.

## Health check: "ipify through the node"
Blacklisting blocks *inbound* traffic from users, so the check has to start where users are (the orchestrator) and go *through* the node.

The probe, per node, every `probe_interval`:
1. The orchestrator brings up an in-process WireGuard client using `golang.zx2c4.com/wireguard/tun/netstack`, so it needs no root and no kernel interface.
2. The client authenticates as a dedicated **probe peer** (one per interface, kept in the DB with a flag, so every node already has it).
3. The endpoint is the **node's public IP**, not the DNS name.
4. Through the tunnel it sends `GET https://api.ipify.org`.
5. **Healthy** means the returned IP equals the expected egress IP.

Expected egress IP:
- the global **exit node IP** if it is set in the UI;
- otherwise the node's own public IP (true today, because nodes NAT their own traffic).

The same check keeps working once the exit-node feature exists.

**Second signal:** a heartbeat that each node writes into the replicated `ha_nodes` table. A stale heartbeat means the node is dead or cut off from the cluster.

**Auto-failover:** promote when the active node fails `fail_threshold` consecutive probes (default 3) and a healthy standby exists. The standby with the highest priority wins. Guards:
- `min_failover_interval` (default 5m) stops flapping;
- fail-back is never automatic;
- an admin can force a promotion from the UI or API at any time.

## DNS: reg.ru updater
- Uses the reg.ru API v2 (`https://api.reg.ru/api/regru2/`). Steps:
  1. `zone/add_alias` adds the new A record.
  2. `zone/remove_record` removes the old A record.
- Doing add-then-remove means the name never resolves to nothing.
- Config: `ha.dns.regru.username`, `password` (from env), `zone`, `subdomain`.
- **reg.ru requirements:** API access must be enabled, and the caller's IP must be on the account's API IP whitelist.
- **TTL:** set a low zone TTL once, via `zone/update_soa` or the panel.
- I must verify these method names and parameters against the current reg.ru docs before coding.
- There is a small Go interface `DNSUpdater{ SetA(ctx, ip) error }` with one implementation, `regru`. Shortcut: it is reg.ru only; add other providers when needed.

## Node registration ("hit /register with a key")
**Prerequisite:** the orchestrator is the first Galera member and bootstraps the cluster. On first start it creates a cluster CA. The CA key is stored in `HaState`, encrypted with the existing gorm encryption serializer (`internal/app/gorm_encryption.go`), so a replacement orchestrator can still sign certificates.

**Steps:**
1. **Admin creates a join token.** In the HA page, click "Create join token". The token is shown once, stored hashed, and has an expiry and a use count. It is a dedicated token, not the admin API token, because it ends up in node env files.
2. **Admin starts the node.** The admin runs the node compose file with `WG_PORTAL_HA_ORCHESTRATOR_URL` and `WG_PORTAL_HA_JOIN_TOKEN` set. The compose file has three services:
   - `join` runs `wg-portal -join` once. It only runs if `/data/ha/joined` does not exist yet.
   - `mariadb` depends on `join` with `service_completed_successfully`.
   - `wg-portal` (role=node) depends on `mariadb`.
3. **`-join` calls the orchestrator.** It sends `POST https://orchestrator.example.org/api/v1/ha/register` with `Authorization: Bearer <join token>` and the body `{node_id: hostname, public_ip, priority}`. If `public_ip` is empty, the node gets it from ipify, the same helper the probe uses.
4. **The orchestrator answers.** It validates the token and inserts the `HaNode` row. It returns:
   - a node TLS cert and key signed by the cluster CA, plus the CA cert;
   - the `wsrep_cluster_address` (all known member IPs);
   - the DB user and password;
   - the `encryption_passphrase`;
   - the WG listen ports the node must open.
5. **The node writes the files.** `-join` writes `galera.cnf`, the certs and `config.yml` into `/data/ha/`, then exits.
6. **MariaDB joins and syncs.** MariaDB joins the cluster and receives SST. wg-portal starts, and its reconcile loop brings up `wg0` with all peers. The node is now a hot standby.

**How Galera is connected:**
- It uses Galera **mutual TLS over the public IPs** (ports 4567, 4568 and 4444), not a separate mgmt mesh. A mesh would need per-node keys, and wg-portal's replicated interfaces share one key. Certificate auth replaces the mesh.
- If one link is blocked (for example orchestrator to node A), Galera relays through the remaining members.

**Removing a node:** a "Remove" button deletes the `HaNode` row. Shortcut: certificates are not revoked; rotate the CA if a node is compromised.

## Client re-resolve (unchanged from the previous draft)
- `PersistentKeepalive` is optional: stale handshakes trigger the re-resolve on idle tunnels too.
- **Linux:** wireguard-tools `reresolve-dns.sh` run by a systemd timer every 30s.
- **Windows:** a Scheduled Task. When `wg.exe show <tun> latest-handshakes` is older than 135s, it runs `wg.exe set <tun> peer <pk> endpoint vpn.<domain>:<port>`. First check whether the current WireGuard for Windows already does this.
- **Mobile:** toggle the tunnel off and on.

## Code changes
0. **Design record:** write this plan to `docs/design/ha.md` first. Include a "Rationale" section with:
   - why a central orchestrator: it is a single point of failure only for admin work;
   - why Galera with the orchestrator as a member: quorum without `garbd`;
   - why the probe runs from the orchestrator through the node to ipify: blacklisting is inbound and regional;
   - why mutual TLS rather than a mesh: replicated interfaces share one key;
   - why all nodes stay hot: promotion = DNS + stats ownership only;
   - the rejected alternatives: identical portal on every node; lease election.

   Keep the document updated as the implementation diverges.
1. **Config**, new file `internal/config/ha.go`, wired into the root config:
   - `role`, `node_id`, `public_ip`, `priority`
   - `heartbeat_interval`, `reconcile_interval`, `probe_interval`, `fail_threshold`, `min_failover_interval`
   - `dns.regru.*`, `on_promote` hook
2. **DB**, in `internal/adapters/database.go`:
   - New models: `HaNode` (node_id, public_ip, priority, last_heartbeat, last_probe_ok, consecutive_fails) and `HaState` (single row: active_node_id, exit_ip, changed_at, changed_by).
   - Add them to AutoMigrate, plus schema step 5 in `migrate()` (line 222).
   - Mark the probe peer with a column on `Peer`, e.g. `IsHaProbe bool`.
3. **Orchestrator role:**
   - Register a DB-only controller in place of `local` in `ControllerManager.registerLocalController` (`internal/app/wireguard/controller_manager.go:59`). It is a new `wgcontroller/dbonly.go`: Save/Delete are no-ops, Get* return empty. UI CRUD then writes only the DB.
   - Force `restore_state=false`, `import_existing=false`, no statistics collector.
   - When an interface is created, generate its probe peer.
4. **Node role:**
   - Heartbeat loop: upsert `HaNode`.
   - Reconcile loop: call the existing `Manager.RestoreInterfaceState(ctx, true)` every `reconcile_interval`.
     - Skip the run when the DB is unreadable; a Galera non-primary member refuses reads.
     - It must never tear interfaces down because of a DB error. Read what the error path at line 204+ writes, and guard it.
   - Statistics collector (`internal/app/wireguard/statistics.go`): skip the tick unless `HaState.active_node_id == node_id`.
   - No expiry or LDAP jobs (`wireguard.go:301`, `users/ldap_sync.go:18`). Those run on the orchestrator.
   - Web: listen on localhost only.
   - Join: a `-join` flag in `cmd/wg-portal/main.go`, next to the existing `-migrateFrom`, implemented in `internal/app/ha/join.go`.
5. **`internal/app/ha/`** (orchestrator only):
   - `probe.go`: netstack client, then the ipify comparison.
   - `failover.go`: the decision loop and `Promote(nodeId, actor)`, which:
     1. updates `HaState`;
     2. calls `DNSUpdater.SetA`;
     3. runs `on_promote`;
     4. writes an audit entry through the existing audit bus topic.
   - `regru.go`: the DNS updater.
   - `ca.go`: create the cluster CA and sign node certs, using the standard library's `crypto/x509`.
   - `register.go`: the v1 handler `POST /api/v1/ha/register`. It authenticates with the join token, not the v1 basic-auth middleware.
   - New model `HaJoinToken` (hash, expires_at, uses_left), added to step 2's migration.
6. **API**: v0 handler `NewHaEndpoint` in `internal/app/api/v0/handlers/`, admin only. It follows the pattern of the existing endpoints:
   - `GET /ha` returns the nodes, active node, exit IP and probe results.
   - `PUT /ha/exit-ip`
   - `POST /ha/promote/{nodeId}`
   - `DELETE /ha/nodes/{nodeId}`
   - `POST|GET|DELETE /ha/join-tokens`
7. **Frontend**: a new admin view, `frontend/src/views/HaView.vue`, plus a route and a nav entry, built with the existing `apiWrapper` helper. It shows a node table (health, last probe, heartbeat age, active badge), Promote and Remove buttons per node, an exit-IP field, and a join-token panel (create / revoke, plus a copyable `docker compose` command).
8. **Docs**: `docs/documentation/getting-started/high-availability.md`, covering:
   - roles
   - Galera with mutual TLS, plus the node compose file with the `join` service
   - reg.ru setup (API whitelist, TTL)
   - client re-resolve scripts
   - orchestrator recovery runbook
   - the requirement that `encryption_passphrase` is identical on all parties
   - rolling upgrades, one party at a time, so the AutoMigrate DDL never runs concurrently

## Checks during implementation
- **Primary keys:** every gorm table must have one, because Galera needs it (check `PeerStatus`, `InterfaceStatus`, `AuditEntry`).
- **Counter reset after promotion:** `UpdatePeerStatus` (`database.go:1098`) must treat the new active node's kernel counters starting at 0 as a reset.
- **Probe-peer clutter:** keep the probe peer out of user-facing peer lists and expiry, or simply name it clearly.
- **New dependency:** netstack pulls in gvisor, which makes the binary larger. The alternative is a kernel `wgprobe` interface on the orchestrator, which needs NET_ADMIN.

## Out of scope (your "future")
- **Exit-node routing**, meaning nodes send all non-peer traffic to an exit node. This plan only stores `exit_ip` and uses it in the probe.
- No shared web sessions: a new orchestrator logs admins out.

## Verification
- **Unit tests:**
  - failover decision: threshold, `min_failover_interval`, no fail-back, chooses the highest-priority healthy node;
  - the regru client against an `httptest` server;
  - the probe comparison logic.
  - token validation: expired, used up, or wrong token is rejected;
  - a cert signed by the CA verifies against the CA.
- **Lab A, `test/ha-lab/docker-compose.yml`** (Docker in WSL, automated):
  - Containers: orchestrator, node-a, node-b, linux-client, coredns, and fake-regru.
  - fake-regru is a tiny Go `httptest`-style server. It implements `add_alias`/`remove_record` and writes a hosts file that CoreDNS reloads.
  - The client resolves `vpn.lab` through CoreDNS.
  - First check that the WSL2 kernel has the `wireguard` module (`modprobe wireguard`). If it doesn't, fall back to Lab B.
  - A `run.sh` script drives the scenarios below.
- **Lab B, Vagrant (`test/ha-vagrant`):** orchestrator, node-a and node-b as Linux VMs, plus one Windows client box, to test the Windows re-resolve Scheduled Task. Use the same fake-regru + CoreDNS, or a real test subdomain at reg.ru.
  - **Hyper-V provider.** VirtualBox on a host with Hyper-V on (WSL2) runs in its slow NEM mode, and VMs failed to boot or froze.
  - **Real-host issue it caught:** Docker's `FORWARD DROP` policy blocks tunnel traffic unless `PostUp` allows forwarding (see the HA guide).
  - **Result (2026-11-14, all passed):**
    - Join and bootstrap worked, with both clients egressing through node-a via real NAT.
    - The block was applied on node-a: it drops all incoming WireGuard UDP. A Windows Firewall rule on the client does not stop WireGuard for Windows.
    - node-b was promoted after 50 s, and the DNS record followed at once.
    - The Linux client (systemd timer) switched 150 s after the block, and the Windows client (scheduled task) after 158 s. Both are bounded by the 135 s stale-handshake threshold.
    - There was no automatic fail-back, and a manual promote moved the record back.
  - **Routing (exit nodes, bypass/blackhole)**, the `run.ps1` section after the HA scenario. Results: TBD.
    - Setup: exit hosts exit-a/exit-b (.20/.21) are wg-quick peers with the re-resolve timer that masquerade out of their lab NIC. Infra has extra targets .57-.61, and CoreDNS serves `policy.test` (`direct`, `blocked`, `both`). The client resolves it through the node's DNS forwarder (upstream .53).
    - No exit: egress is node-a. With exit-a, then exit-b active, egress is .20, then .21. The exit check is ok, and the nodes stay healthy. With none, egress is the node again.
    - Lists, with exit-a active: bypassed IP and domain leave through the node, and other traffic through exit-a. Blackholed IP and domain are rejected at once. A domain in both lists is rejected. Disabling the blackhole list lets it through the exit.
    - exit-a down: the exit check fails, with no failover. Up again: ok.
    - No deadlock: node-a blocked while exit-a is active → node-b promoted. The client and exit-a re-resolve on their own, and egress returns to exit-a through node-b.
- **Scenarios:**
  - **Join:** start node-b with only a join token. It must join Galera, show up in the UI, and have `wg0` up with all peers. A reused or expired token must get a 401.
  - Block UDP to node A from the orchestrator with `iptables`. After 3 probes, B must be promoted, the reg.ru A record must change (`dig @ns1.reg.ru`), and the Linux and Windows clients must reconnect.
  - Click Promote back to A in the UI. The record must flip back and an audit entry must exist.
  - Create a peer in the UI. It must appear in `wg show` on both nodes within `reconcile_interval`.
  - Stop the orchestrator. Clients must stay connected and stats keep updating in the DB on the active node. Start a new orchestrator, join Galera, and the UI must show current state.

## Rationale

**Why a central orchestrator, not a copy of the portal on every node.**
- Users only need a working WireGuard node. The UI and control plane matter only to the admin.
- If the orchestrator dies, the system is degraded, not failed: no peer CRUD, no expiry, no failover, but tunnels keep working.
- The thing that actually fails in practice is a node IP getting blacklisted. A separate orchestrator (in the users' region) can observe that; a node can't observe its own inbound blacklisting.
- The full DB replica on every node makes the orchestrator disposable: start a new one, let it join Galera, and move the portal DNS name to it.

**Why MariaDB Galera, with the orchestrator as a member.**
- wg-portal already supports MySQL/MariaDB, so the app needs no DB code changes.
- Multi-master means every node holds a full, readable local copy.
- The orchestrator counts as a member, so a dyad of nodes is 3 members and has quorum without `garbd`.
- Quorum also fences a partitioned minority: it cannot commit.

**Why the probe goes orchestrator → node → ipify.**
- Blacklisting is inbound and regional. A check run from the node itself, or from outside the users' region, would report a blocked node as healthy.
- Comparing the egress IP with the expected exit IP checks the whole user path: reachability, the handshake, forwarding and NAT/exit.

**Why mutual TLS for Galera, not a management WireGuard mesh.**
- wg-portal interfaces are replicated, so every node shares the same interface key. A mesh needs a distinct key per node.
- Certificates from a cluster CA the orchestrator owns also make `/register` simple.

**Why all nodes stay hot.**
- Every node already has the same key, port and peers from the replicated DB, so promotion = rewrite DNS + move stats ownership.
- A client with stale DNS that hits a standby still gets a working tunnel.

**Why periodic reconcile, not a cross-node event bus.**
- `RestoreInterfaceState` already applies the DB to the kernel idempotently.
- Running it on a timer reuses tested code. The cost is a propagation delay of `reconcile_interval`.

**Rejected alternatives.**
- *Identical portal on every node behind a DNS flip:* it makes every node a control-plane target and needs leader election plus shared sessions, and it can't detect regional blacklisting.
- *DB lease election among nodes:* unnecessary, because the orchestrator decides. `GET_LOCK` isn't replicated by Galera anyway.
- *Floating IP / VRRP:* the nodes sit at different sites and clouds, so it isn't possible.

## Implementation notes (where the code differs from the plan above)

**Node reconcile.** Nodes use a kernel-only reconciler (`internal/app/wireguard/ha_reconcile.go`), not `RestoreInterfaceState`.
- `RestoreInterfaceState` writes to the DB: a fresh node would mark the interface "disabled / missing" in the *replicated* DB, and the other nodes would then tear down their peers.
- The reconciler skips work when a fingerprint of the DB rows is unchanged.
- It runs `PreUp`/`PostUp` only when it creates an interface.
- On a DB error (e.g. a Galera non-primary member) it keeps the current kernel state.

**Join flag.** The join command is `wg-portal -ha-init`, not `-join`.
- It runs as an init container before MariaDB, for both roles.
- Orchestrator: creates the CA and its own certificate.
- Node: registers at `POST /api/v1/ha/register` with the join token.
- Both: render `galera.cnf` from `data/ha/members`.
- `-ha-export-ca <dir>` exports the CA from any node's DB copy, to rebuild a lost orchestrator.

**Migration.** `-ha-import <sqlite file>` migrates a temporary copy of a standalone database to the current schema, then copies every table raw into the empty cluster database (`adapters.ImportDatabase`). It does this in one transaction with foreign-key checks off. Encrypted columns stay valid because the passphrase is the same.

**Keepalive.** `PersistentKeepalive` is not needed for client failover: on an idle tunnel the handshake goes stale anyway, so the re-resolve scripts keep the endpoint current.

**Cluster CA.** The CA lives in `data/ha/ca*.pem` on the orchestrator. The orchestrator copies it into `HaState` on startup, with the key encrypted by the existing `encstr` serializer.

**Admin automation.** In addition to the UI (v0 `/ha/*`), admins can use v1 endpoints with basic auth: `GET /ha/status`, `POST /ha/promote/{id}` and `POST /ha/join-tokens`.

**Probe.**
- Shape: an in-process netstack WireGuard client authenticates as one probe peer per server interface (table `ha_probe_peers`).
- Endpoint: it dials the node's public IP and the interface listen port.
- Health check: it compares the egress IP with the node's own public IP. The exit-IP setting (`HaState.ExitIp`, `PUT /ha/exit-ip`) was removed when exit routing was implemented, see below.

**Exit nodes and routing policy** (user guide: `docs/documentation/getting-started/routing-policy.md`).
- The active exit is the exit candidate (`exit_candidates`) whose server-side extra allowed IPs contain `0.0.0.0/0`. Candidates and the marker live in the DB, so they replicate. Selecting an exit moves the marker through the WireGuard manager's peer update path, so the orchestrator stays DB-only and nodes pick it up on reconcile.
- In exit mode the route manager strips default routes, because wg-quick style routing would send the node's own traffic (Galera, SSH) through the exit. `app/routing` instead installs rules scoped to `iif wg0` (prefs 5200 bypass mark, 5210 node probe peer, 5220 exit table `0x57000000 + ifindex`), ipsets and the `WGP-FWD`/`WGP-MARK` chains via `adapters/policyroute`. It runs on nodes and standalone, never on the orchestrator.
- **The node probe skips the exit** (rule 5210): an exit host only has a tunnel to the active node, so a probe through a standby would never pass and failover would deadlock. A second probe peer, `ha-probe-exit`, is routed like a client: each cycle the orchestrator probes through the active node and compares with the active candidate's `EgressIp` (`HaState.ExitCheck*`). It is informational and never triggers a failover. Shortcut: only the first server interface is checked.
- Lists and the DNS forwarder settings are DB rows too, so every node applies the same policy. Each node runs its own DNS forwarder on the tunnel address.

**Promote order.** DNS first, then DB state, so a failed reg.ru call leaves the state unchanged and the next run retries. Adding the new record, then removing the old ones, avoids an empty answer during the switch.

**Leader-only work.**
- Nodes do not run peer expiry or LDAP sync.
- The statistics collector skips its ticks unless this node is the active one.
- The orchestrator collects no statistics and uses a DB-only `local` controller.

**Fixes found along the way:**
- **Traffic counters.** `CalculateTrafficDelta` wrapped around when a counter decreased, e.g. after a failover or an interface re-creation. It now treats a decrease as a reset.
- **Startup.** HA parties wait up to `advanced.startup_timeout` for the database at startup, since the first SST can take minutes.

**Verified in `test/ha-lab`** (Docker on WSL2):
- join and token reuse rejection
- bootstrap promotion
- replication to standbys
- automatic failover in about 20s with 5s probes, and the client following via `wg-reresolve.sh`
- changes reaching a node whose direct link to the orchestrator is cut (Galera relays)
- no automatic fail-back
- manual promotion
- tunnels surviving an orchestrator outage

**Lab limits:**
- The WSL2 kernel has no `nf_nat`, so node egress is emulated with a socat sidecar instead of a MASQUERADE `PostUp` rule.
- Blacklisting is simulated with blackhole routes.
- In this Docker lab the Windows re-resolve script is only tested against a stub `wg.exe`. The Hyper-V lab (`test/ha-vagrant`) covers real NAT and the real Windows client.
