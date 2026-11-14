WireGuard Portal can send the internet traffic of its peers through an **exit node** and route selected
destinations differently:

- **Exit node.** A peer of the server interface (for example a VPS in another country) through which all client
  traffic leaves. You choose a short list of **exit candidates** once (hot standbys) and then switch the
  **active** exit, or none, with one click.
- **Bypass lists.** Destinations that leave directly through the VPN server instead of the exit.
- **Blackhole lists.** Destinations that are rejected. Blackhole always wins over bypass.

Lists contain IPs, CIDRs and domain names. Domains are matched by a built-in DNS forwarder.

The policy is applied by every wg-portal that runs WireGuard in the kernel: a standalone instance with the
`local` backend, or the nodes of an [HA cluster](high-availability.md). The HA orchestrator only stores the
settings; they replicate to the nodes with the database. Switching is manual only.

## How it works

An exit node is a peer whose server-side *extra allowed IPs* contain `0.0.0.0/0` (and `::/0`). That route is
the marker for the active exit: selecting an exit moves it from all peers of the interface to the chosen
candidate. An interface with at least one candidate is in **exit mode**:

- wg-portal does **not** install the default route wg-quick style (that would send the server's own traffic,
  database replication and SSH replies through the exit). Only traffic that arrives from the tunnel uses the
  exit.
- A peer that is not a candidate cannot be saved with `0.0.0.0/0` or `::/0` in its extra allowed IPs.

Interfaces without candidates behave as before. In that case a peer carrying `0.0.0.0/0` gets the wg-quick style
default route, which takes **all** of the server's own traffic.

**Detected default routes.** A peer that carries `0.0.0.0/0` or `::/0` but is not a candidate (typically
configured before exit nodes existed) is listed as *detected* on the routing page, and in `Detected` of
`GET /routing/exits`. Nodes also log a warning. To adopt it, use *Add as candidate*, enter its egress IP and save.
The interface then enters exit mode, and the peer is the active exit, because it already has the marker. Every
server re-syncs its routes when an interface enters or leaves exit mode, so HA nodes drop the system-wide route
within one apply interval, even though the peer itself did not change.

A packet from a peer (`iif wg0`) passes:

```
filter FORWARD   -> chain WGP-FWD:  dst in wgp-block{4,6} or wgp-block-dns{4,6}   -> REJECT
mangle PREROUTING -> chain WGP-MARK: dst in wgp-bypass{4,6} or wgp-bypass-dns{4,6} -> MARK 0x10000000
ip rules:
  pref 5200  iif wg0 fwmark 0x10000000/0x10000000 lookup main     (bypass)
  pref 5210  iif wg0 from <HA probe peer>          lookup main     (HA health probe skips the exit)
  pref 5220  iif wg0                               lookup <exit>   (only while an exit is active)
table <exit> = 0x57000000 + interface index:  default dev wg0
```

WireGuard then hands the packet to the exit peer, because it owns `0.0.0.0/0`. Traffic to other peers still
matches their more specific allowed IPs. Without an active exit there is no rule 5220: traffic leaves through
the server as usual, and only the blackhole lists apply.

Rejected TCP connections get a reset, everything else an ICMP *administratively prohibited*, so clients fail
fast instead of timing out. Traffic to the server itself (for example its DNS forwarder) is never rejected.

When there are no candidates and no enabled lists, wg-portal removes all chains, sets, rules and exit routes.

## Setting up an exit host

The exit host is a normal WireGuard client of the server interface.

1. **Create a peer** for it in the UI. Its *allowed IPs* (the client side) must be the VPN subnet only, for
   example `10.11.12.0/24`, never `0.0.0.0/0`: the exit host must not route its own traffic into the tunnel.
2. **Install the config** with `wg-quick` and add forwarding and NAT to it. Replace `eth0` with the uplink:

    ```
    [Interface]
    ...
    PostUp   = sysctl -w net.ipv4.ip_forward=1
    PostUp   = iptables -A FORWARD -i %i -j ACCEPT; iptables -A FORWARD -o %i -j ACCEPT
    PostUp   = iptables -t nat -A POSTROUTING -s 10.11.12.0/24 -o eth0 -j MASQUERADE
    PostDown = iptables -D FORWARD -i %i -j ACCEPT; iptables -D FORWARD -o %i -j ACCEPT
    PostDown = iptables -t nat -D POSTROUTING -s 10.11.12.0/24 -o eth0 -j MASQUERADE

    [Peer]
    ...
    AllowedIPs = 10.11.12.0/24
    PersistentKeepalive = 25
    ```

    `PersistentKeepalive` lets the server reach the exit host behind NAT before the exit host sends anything.
    For IPv6 exits, enable `net.ipv6.conf.all.forwarding` and add the `ip6tables` equivalents.
3. **HA clusters:** the exit host connects to the VPN DNS name like any client, so it must re-resolve it after
   a failover. Install the re-resolve timer from `deploy/ha/clients` (see
   [Clients](high-availability.md#3-clients)). Until it re-resolves, clients have no internet through the exit.

!!! tip
    On the VPN server, restrict its own masquerade rule to the uplink as well
    (`-t nat -A POSTROUTING -s 10.11.12.0/24 -o eth0 -j MASQUERADE`). Otherwise traffic towards the exit is
    masqueraded to the server's tunnel address and the exit host sees every client as the server.

## Choosing the exit

In the UI, open **Routing** (admin menu):

1. Under *Exit nodes*, add the exit peers of the interface as **candidates** and enter each one's public
   **egress IP** (the address the internet sees). HA uses it for the exit check.
2. Select the **active** exit, or **None**. Removing the active exit from the candidates deactivates it.

The change is saved like a normal peer change: a standalone instance applies it at once, HA nodes within
`ha.reconcile_interval`. Each selection writes an audit entry.

Traffic from the clients to private networks behind the server (for example its LAN) also goes to the exit
while one is active. Add such networks to a bypass list.

## Lists

Lists are uploaded on the *Routing* page, one file per list, or pasted. Each list is a **bypass** or a
**blackhole** list and can be disabled without deleting it. Lists apply to all server interfaces.

Format, one entry per line:

```
# comments start with "#"
192.0.2.10              # an IP
198.51.100.0/24         # a CIDR
example.com             # a domain: matches example.com and all its subdomains
*.example.net           # a leading "*." or "." is accepted
0.0.0.0 ads.example.org # hosts file lines (0.0.0.0, 127.0.0.1, ::, ::1) take the domain names
```

Public blocklists in hosts file format can be uploaded as-is: hosts lines of other addresses (such as
`255.255.255.255 broadcasthost`) and names without a dot (such as `localhost`) are ignored. If any line is
invalid, the whole list is rejected, and the first 10 errors are shown with their line numbers.
`0.0.0.0/0` and `::/0` are not allowed in lists.

**Blackhole wins.** An address in both kinds is rejected. A domain in a blackhole list is never bypassed,
even if a bypass list contains it or one of its parents.

## Domain rules: the DNS forwarder

Domains work through the built-in DNS forwarder:

1. Enable it under *DNS forwarder* and enter one or more **upstreams** (`host[:port]`, tried in order with a
   2 second timeout each).
2. Set the interface's **peer DNS** to the server's tunnel address, for example `10.11.12.1`, and hand out new
   client configs (or *Apply peer defaults*).

The forwarder listens on UDP and TCP port 53 of every server interface address. When the question name, or any
name in the CNAME chain of the answer, matches a list domain, the answer's A/AAAA records are added to the
`wgp-bypass-dns` or `wgp-block-dns` set. Entries expire with the record TTL, at least 60 seconds and at most
one day. When a domain is removed from a list (or its list is disabled), the learned addresses of that kind are
flushed; clients that still cache an answer are routed normally until they query again.

Limits:

- Clients that use DNS over HTTPS or a hardcoded DNS server bypass domain rules (IP and CIDR rules still apply).
- A blackholed domain still resolves; its connections are rejected.

### Pi-hole as upstream

A Pi-hole (or any other filtering resolver) can serve as upstream when it is a peer of the VPN:

1. Create a peer for the Pi-hole host and connect it like any client, for example as `10.11.12.53`.
2. Let the Pi-hole answer queries from the tunnel (in Pi-hole: *Settings → DNS → Permit all origins*, or
   listen on the WireGuard interface).
3. Set the forwarder upstream to `10.11.12.53`.

The server's own queries reach the Pi-hole through the tunnel route; they never use the exit. Caveats:

- The Pi-hole sees all queries as coming from the VPN server, not from the individual clients.
- **HA:** the Pi-hole is connected to the active node only. After a failover, DNS through the new active node
  waits until the Pi-hole re-resolves the VPN name (install the re-resolve timer on it as well). A second
  upstream, for example a public resolver, serves as fallback, at the cost of bypassing the Pi-hole's filtering
  while it is used.

## Troubleshooting

On the VPN server (or HA node):

```
ip rule                                  # prefs 5200, 5210, 5220
ip route show table $((0x57000000 + $(cat /sys/class/net/wg0/ifindex)))
ipset list -t                            # wgp-block4, wgp-block-dns4, wgp-bypass4, ... and their sizes
iptables -S WGP-FWD
iptables -t mangle -S WGP-MARK           # and the ip6tables equivalents
```

With `advanced.log_level: debug`, wg-portal logs every applied command, rule and route; failed commands are
logged with their output.

Requirements and notes:

- The `iptables` binaries of the container and the host must use the same backend (both nft, the default on
  current distributions), and the kernel needs `ip_set`, `xt_set` and `xt_MARK`, which Debian and Ubuntu
  kernels ship.
- Our chains are jumped from position 1 of `FORWARD` and `PREROUTING`. wg-portal moves the jumps back to the
  top when a `PostUp` hook inserts rules before them.
- The exit table number is `0x57000000 + interface index`, far above wg-portal's own tables
  (`advanced.route_table_offset + interface index`). A collision is reported as an error.
- Very large lists are limited by the database's `max_allowed_packet` (16 MB by default on MariaDB).
