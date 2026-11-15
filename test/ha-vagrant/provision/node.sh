#!/bin/bash
# HA node from deploy/ha, joined with the token the orchestrator provisioning wrote.
# usage: node.sh <node id> <public ip> <priority>
set -euo pipefail
token=$(curl -sf http://192.168.77.10:8099/join-token)

mkdir -p /opt/wg-portal
cd /opt/wg-portal
cp /repo/deploy/ha/docker-compose.ha-node.yml /repo/test/ha-vagrant/node.override.yml .
cat > .env <<EOF
WG_PORTAL_IMAGE=wg-portal:ha-lab
WG_PORTAL_HA_NODE_ID=$1
WG_PORTAL_HA_PUBLIC_IP=$2
WG_PORTAL_HA_PRIORITY=$3
WG_PORTAL_HA_ORCHESTRATOR_URL=http://192.168.77.10:8888
WG_PORTAL_HA_JOIN_TOKEN=$token
EOF
docker compose -f docker-compose.ha-node.yml -f node.override.yml up -d

# joined when the replicated interface is up with its peers (2 clients + the probe peer)
for _ in $(seq 100); do
  [ "$(wg show wg0 peers 2>/dev/null | wc -l)" -ge 3 ] && break
  sleep 3
done
wg show wg0 | head -4
[ "$(wg show wg0 peers | wc -l)" -ge 3 ]
iptables -t nat -S POSTROUTING | grep -q MASQUERADE # PostUp ran with real kernel NAT
iptables -S FORWARD | grep -q -- '-i wg0 -j ACCEPT' # and opened forwarding past Docker's DROP policy
echo "$1 joined"
