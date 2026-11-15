#!/bin/bash
# HA node from deploy/ha, joined with the token the orchestrator provisioning wrote.
# usage: node.sh <node id> <public ip> <priority> [node that must have joined first]
set -euo pipefail
# the VMs come up in parallel: the orchestrator serves the token once its provisioning is done
for _ in $(seq 400); do token=$(curl -sf http://192.168.77.10:8099/join-token) && break; sleep 3; done
[ -n "${token:-}" ] || { echo "no join token from the orchestrator" >&2; exit 1; }
# Galera: one joiner at a time. Joined means the orchestrator probes the earlier node successfully
if [ -n "${4:-}" ]; then
  for _ in $(seq 400); do
    curl -sf -u admin@wgportal.local:lab-admin-api-token-123456 http://192.168.77.10:8888/api/v1/ha/status |
      python3 -c "import json,sys; sys.exit(not any(n['NodeId'] == '$4' and n['LastProbeOk'] for n in json.load(sys.stdin)['Nodes']))" &&
      break
    sleep 3
  done
fi

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

# joined when the replicated interface is up with its peers (2 clients + the probe peer). It lives in the network
# namespace of the net service, not on the host
in_node() { docker compose -f docker-compose.ha-node.yml exec -T wg-portal "$@"; }
for _ in $(seq 100); do
  [ "$(in_node wg show wg0 peers 2>/dev/null | wc -l)" -ge 3 ] && break
  sleep 3
done
in_node wg show wg0 | head -4
[ "$(in_node wg show wg0 peers | wc -l)" -ge 3 ]
in_node iptables -t nat -S POSTROUTING | grep -q MASQUERADE # PostUp ran with real kernel NAT
in_node iptables -S FORWARD | grep -q -- '-i wg0 -j ACCEPT'
! wg show wg0 >/dev/null 2>&1 # and nothing on the host: no host networking
echo "$1 joined"
