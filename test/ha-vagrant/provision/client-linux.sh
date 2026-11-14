#!/bin/bash
# Linux client: wg-quick tunnel to vpn.lab.test plus the systemd re-resolve timer from deploy/ha/clients.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
command -v wg >/dev/null || { apt-get update -q; apt-get install -y -q wireguard-tools curl iptables; }

# resolve lab.test via the lab DNS (not persistent across reboots, re-run provisioning after a reboot)
iface=$(ip -o -4 addr show | awk '/192\.168\.77\.100\//{print $2}')
resolvectl dns "$iface" 192.168.77.53
resolvectl domain "$iface" '~lab.test'

# wg setconf retries an unresolvable endpoint for a very long time, so fail fast instead
for _ in $(seq 40); do resolvectl query vpn.lab.test >/dev/null 2>&1 && break; sleep 3; done
resolvectl query vpn.lab.test || { echo "vpn.lab.test does not resolve: no active node?" >&2; exit 1; }

curl -sf -o /etc/wireguard/wg0.conf http://192.168.77.10:8099/client-linux.conf
chmod 600 /etc/wireguard/wg0.conf
systemctl enable --now wg-quick@wg0
# policy.test via the node's DNS forwarder, so domain bypass/blackhole rules see the lookups (lost when the
# tunnel restarts; run.ps1 sets it again before the routing checks)
resolvectl dns wg0 10.11.12.1
resolvectl domain wg0 '~policy.test'

install -m 755 /repo/deploy/ha/clients/wg-reresolve.sh /usr/local/bin/wg-reresolve.sh
install -m 644 /repo/deploy/ha/clients/wg-reresolve@.service /repo/deploy/ha/clients/wg-reresolve@.timer \
  /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now wg-reresolve@wg0.timer
wg show wg0
