# HA lab on Hyper-V VMs

This lab complements `test/ha-lab` (Docker, fast). It runs everything on real VMs:

- the real `deploy/ha` compose files;
- real kernel NAT, behind Docker's `FORWARD DROP` policy just like a production node;
- a Linux client using the `wg-reresolve@wg0` systemd timer;
- a Windows 11 client using the WireGuard for Windows tunnel service and the scheduled re-resolve task.

| VM | IP | Role |
|----|----|------|
| infra | 192.168.77.50, .53, .57-.61 | fake ipify (:8080), fake reg.ru API (:8081), CoreDNS for `lab.test` and `policy.test` |
| orchestrator | 192.168.77.10 | `docker-compose.ha-orchestrator.yml`; UI at http://192.168.77.10:8888 |
| node-a / node-b | 192.168.77.11 / .12 | `docker-compose.ha-node.yml`, joined with a join token |
| exit-a / exit-b | 192.168.77.20 / .21 | exit hosts: wg-quick peers `exit-a`/`exit-b` (tunnel allowed IPs `10.11.12.0/24` only), forwarding, masquerade out of the lab NIC, re-resolve timer |
| client-linux | 192.168.77.100 | wg-quick, re-resolve timer |
| client-win | 192.168.77.101 | WireGuard for Windows, re-resolve scheduled task |

Each VM has two NICs:

- the Hyper-V "Default Switch", which provides DHCP, internet and `vagrant ssh`;
- the internal `wg-ha-lab` switch, which carries the static lab IPs. The host is 192.168.77.1.

There are no synced folders: `run.ps1` uploads a bundle, and later VMs fetch the join token and their client
configs from the orchestrator at `:8099`.

## Requirements

- Windows host with Hyper-V enabled. It already is if you use WSL2. VirtualBox is not used.
- Vagrant 2.4
- Docker in WSL, used to build the image
- Go
- Disk space: about 25 GB with the Windows VM. The Linux VMs are differencing disks of the box.

## Usage

Once, from an elevated PowerShell, run the setup below. It adds you to "Hyper-V Administrators" and creates the
switch. Then sign out and back in.

```powershell
.\test\ha-vagrant\setup-hyperv.ps1
```

Then, as your normal user:

```powershell
.\test\ha-vagrant\run.ps1               # build, vagrant up, run the failover scenario
.\test\ha-vagrant\run.ps1 -NoWindows    # without the Windows VM
.\test\ha-vagrant\run.ps1 -SkipHa       # bootstrap and the routing scenario only
.\test\ha-vagrant\run.ps1 -Rebuild      # rebuild the image after code changes
.\test\ha-vagrant\run.ps1 -Destroy      # remove the VMs (setup-hyperv.ps1 -Remove deletes the switch)
```

For manual `vagrant` commands, set the same environment variables that `run.ps1` sets:

- `VAGRANT_CWD` to this folder;
- `VAGRANT_DEFAULT_PROVIDER=hyperv`;
- `VAGRANT_EXPERIMENTAL=typed_triggers`.

The trigger that attaches the lab NIC needs the last one.

After `-Rebuild`, push the new image to the running VMs with `vagrant provision`.

The admin login is `admin@wgportal.local` / `lab-admin-password-123`.

## Scenario

1. The highest priority node becomes active, and DNS and both clients use it.
2. node-a drops all incoming WireGuard traffic, which simulates a blacklisted node. The block sits on the
   node, because a Windows Firewall rule on the client does not stop WireGuard for Windows.
3. The orchestrator promotes node-b and the reg.ru record follows. The clients switch on their own, through
   the timer or the scheduled task.
4. The block is lifted. node-b stays active (no automatic fail-back), then a manual promotion moves the
   record back to node-a.

## Routing scenario

Runs after the HA scenario, against the Linux client. It starts by resetting what an earlier run left behind: no
active exit, no candidates, no lists named `lab-*`.

Setup from provisioning:

- the peers' allowed IPs include `192.168.77.56/29`, so the extra infra addresses .57-.61 go through the tunnel
  (the lab DNS .53 does not);
- the DNS forwarder is on, with upstream 192.168.77.53;
- the client resolves `policy.test` through the node's forwarder (`resolvectl dns wg0 10.11.12.1`). CoreDNS serves
  that zone from `policy.hosts`: `direct` → .59, `blocked` → .60, `both` → .61.

The routing endpoints are in the UI API (v0), which takes no API token. `run.ps1` and `orchestrator.sh` log in
with the admin password and send the session's CSRF token in `X-CSRF-TOKEN`.

1. No exit: the client's egress, as seen by the fake ipify, is node-a (.11).
2. exit-a and exit-b become candidates, and exit-a is activated. Egress is .20. The exit check is ok, and the nodes
   stay healthy without a failover, because their probes skip the exit.
3. Switching to exit-b gives .21, and "none" gives .11 again.
4. With exit-a active, a bypass list (`192.168.77.57`, `direct.policy.test`, `both.policy.test`) and a blackhole
   list (`192.168.77.58`, `blocked.policy.test`, `both.policy.test`) are uploaded.
   - .57 and `direct` leave through node-a (.11), and other traffic still leaves through exit-a (.20).
   - .58, `blocked` and `both` are rejected at once, because blackhole wins over bypass.
5. With the blackhole list disabled, `blocked` leaves through exit-a.
6. exit-a's tunnel goes down: the exit check fails, and no failover happens. Up again: ok.
7. node-a is blocked while exit-a is active. node-b is promoted, the client and exit-a re-resolve on their own, and
   egress is .20 again through node-b, so there is no deadlock. node-a is then promoted back.
