#!/bin/bash
# Every Linux lab VM: static IPs on the lab NIC (found by its MAC) and the lab bundle unpacked to /repo.
# usage: lab-base.sh <MAC as 00155D4D0032> <ip>...
set -euo pipefail
mac=$(echo "$1" | tr 'A-F' 'a-f' | sed 's/../&:/g; s/:$//')
shift
addrs=$(printf '%s/24, ' "$@")

cat > /etc/netplan/60-lab.yaml <<EOF
network:
  version: 2
  ethernets:
    lab:
      match:
        macaddress: "$mac"
      addresses: [${addrs%, }]
EOF
chmod 600 /etc/netplan/60-lab.yaml # applies from the next boot on
# now: set the addresses directly. netplan apply would also restart eth0, whose Default Switch DHCP lease then
# changes and drops vagrant's SSH session.
dev=$(ip -o link | awk -v m="$mac" '$0 ~ m { sub(":", "", $2); print $2 }')
[ -n "$dev" ] || { echo "no NIC with MAC $mac" >&2; exit 1; }
ip link set "$dev" up
for a in "$@"; do ip addr replace "$a/24" dev "$dev"; done
ip -4 -br addr show "$dev"

# generic/ubuntu2204 ships DNSSEC=yes and hardcoded public resolvers: flaky lookups, and the unsigned lab.test
# zone would fail validation. Use the Default Switch DNS from DHCP without DNSSEC; let apt retry.
mkdir -p /etc/systemd/resolved.conf.d
printf '[Resolve]\nDNS=\nDNSSEC=no\n' > /etc/systemd/resolved.conf.d/lab.conf
systemctl restart systemd-resolved
echo 'Acquire::Retries "5";' > /etc/apt/apt.conf.d/80-lab-retries

rm -rf /repo && mkdir -p /repo
tar -xzf /tmp/lab-bundle.tgz -C /repo
