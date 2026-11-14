#!/bin/bash
# Exit host: a plain WireGuard peer of wg0 that forwards and masquerades the peers' traffic out of its lab NIC,
# plus the systemd re-resolve timer, so it follows the active node after a failover.
# usage: exit.sh <peer name: exit-a | exit-b>
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
command -v wg >/dev/null || { apt-get update -q; apt-get install -y -q wireguard-tools curl iptables; }

# resolve lab.test via the lab DNS (not persistent across reboots, re-run provisioning after a reboot)
lab=$(ip -o -4 addr show | awk '/ 192\.168\.77\./{print $2; exit}')
resolvectl dns "$lab" 192.168.77.53
resolvectl domain "$lab" '~lab.test'
for _ in $(seq 40); do resolvectl query vpn.lab.test >/dev/null 2>&1 && break; sleep 3; done
resolvectl query vpn.lab.test || { echo "vpn.lab.test does not resolve: no active node?" >&2; exit 1; }

# what the admin prepares on a real exit host: forwarding and masquerade out of the uplink (iptables rules are
# not persistent either)
sysctl -q -w net.ipv4.ip_forward=1
echo net.ipv4.ip_forward=1 > /etc/sysctl.d/90-lab.conf
for dir in -i -o; do
  iptables -C FORWARD "$dir" wg0 -j ACCEPT 2>/dev/null || iptables -A FORWARD "$dir" wg0 -j ACCEPT
done
iptables -t nat -C POSTROUTING -s 10.11.12.0/24 -o "$lab" -j MASQUERADE 2>/dev/null ||
  iptables -t nat -A POSTROUTING -s 10.11.12.0/24 -o "$lab" -j MASQUERADE

curl -sf -o /etc/wireguard/wg0.conf "http://192.168.77.10:8099/$1.conf"
chmod 600 /etc/wireguard/wg0.conf
systemctl enable --now wg-quick@wg0

install -m 755 /repo/deploy/ha/clients/wg-reresolve.sh /usr/local/bin/wg-reresolve.sh
install -m 644 /repo/deploy/ha/clients/wg-reresolve@.service /repo/deploy/ha/clients/wg-reresolve@.timer \
  /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now wg-reresolve@wg0.timer
wg show wg0
