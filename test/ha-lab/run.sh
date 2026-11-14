#!/usr/bin/env bash
# End-to-end HA lab (Linux/WSL with Docker and a WireGuard capable kernel):
#   standalone (SQLite) -> import -> join -> bootstrap promotion -> client traffic -> replication -> blacklist failover -> client re-resolve
#   -> manual promote back -> orchestrator outage.
# usage: test/ha-lab/run.sh            (run all scenarios, keep the lab running)
#        test/ha-lab/run.sh down       (remove the lab)
set -euo pipefail

REPO_DIR=$(cd "$(dirname "$0")/../.." && pwd)
LAB=${LAB_DIR:-$HOME/wgp-ha-lab} # docker volumes and bind mounts need a native Linux filesystem
API=http://10.99.0.10:8888/api/v1
SAPI=http://10.99.0.20:8888/api/v1 # standalone instance that gets migrated
AUTH=admin@wgportal.local:lab-admin-api-token-123456
export REPO_DIR JOIN_TOKEN=""

dc() { (cd "$LAB" && docker compose "$@"); }
log() { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
fail() { printf '\033[1;31mFAIL: %s\033[0m\n' "$*"; exit 1; }
api() { curl -sf -u "$AUTH" -H 'Content-Type: application/json' "$@"; }
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
wait_for() { # wait_for <seconds> <description> <command...>
  local timeout=$1 what=$2; shift 2
  local start=$SECONDS
  until "$@" >/dev/null 2>&1; do
    ((SECONDS - start < timeout)) || fail "timeout after ${timeout}s waiting for: $what"
    sleep 2
  done
  echo "ok: $what ($((SECONDS - start))s)"
}
active_node() { api "$API/ha/status" | json "d['State']['ActiveNodeId']"; }
is_active() { [ "$(active_node)" = "$1" ]; }
dns_is() { [ "$(dc exec -T client nslookup vpn.lab.test 10.99.0.53 2>/dev/null | awk '/^Address: /{print $2}' | sort | tr '\n' ' ')" = "$1 " ]; }
client_egress() { dc exec -T client curl -s --max-time 3 http://10.11.12.1:8080/; } # node egress (see compose)
egress_is() { [ "$(client_egress)" = "$1" ]; }
peer_count() { dc exec -T "$1" wg show wg0 peers | wc -l; }
peers_are() { [ "$(peer_count "$1")" -eq "$2" ]; }
# block <netns container> <ip>: route all traffic towards ip into a blackhole (simulates a blacklisted node;
# the WSL2 kernel has no usable iptables). It also cuts Galera between the two, which Galera survives.
block() { docker run --rm --net "container:$1" --cap-add NET_ADMIN alpine:3.20 ip route add blackhole "$2/32"; }
unblock() { docker run --rm --net "container:$1" --cap-add NET_ADMIN alpine:3.20 ip route del blackhole "$2/32"; }

if [ "${1:-}" = "down" ]; then
  dc down -v --remove-orphans
  exit 0
fi

log "preparing $LAB"
mkdir -p "$LAB"
if [ -f "$LAB/docker-compose.yml" ]; then dc down -v --remove-orphans >/dev/null 2>&1 || true; fi
docker run --rm -v "$LAB:/lab" alpine:3.20 rm -rf /lab/run # files are owned by container root
rsync -a --delete --exclude run "$REPO_DIR/test/ha-lab/" "$LAB/"
rsync -a "$REPO_DIR/deploy/ha/initdb/" "$LAB/initdb/"
rsync -a "$REPO_DIR/deploy/ha/clients/" "$LAB/clients/"
mkdir -p "$LAB"/run/{standalone,orch,a,b,dns,client}
echo "REPO_DIR=$REPO_DIR" > "$LAB/.env" # lets you run docker compose in $LAB by hand

log "building image"
dc build orch-init

log "MIGRATION: a standalone wg-portal (SQLite) with an interface and a client"
dc up -d fakes coredns client standalone
wait_for 120 "standalone API" api "$SAPI/interface/all"
wait_for 120 "client tools installed" dc exec -T client test -f /ready
api "$SAPI/interface/prepare" | python3 -c '
import json,sys
i=json.load(sys.stdin)
i.update(Identifier="wg0", Mode="server", ListenPort=51820, Addresses=["10.11.12.1/24"],
         PeerDefNetwork=["10.11.12.0/24"], PeerDefEndpoint="vpn.lab.test:51820",
         PeerDefAllowedIPs=["10.11.12.0/24"], PeerDefPersistentKeepalive=5)
print(json.dumps(i))' > "$LAB/run/iface.json"
api -X POST "$SAPI/interface/new" --data @"$LAB/run/iface.json" >/dev/null
PEER=$(api "$SAPI/peer/prepare/wg0" | python3 -c '
import json,sys
p=json.load(sys.stdin); p["DisplayName"]="lab-client"; print(json.dumps(p))' |
  api -X POST "$SAPI/peer/new" --data @- | json "d['Identifier']")
# the client keeps this config through the whole migration: same keys, same DNS endpoint
api -G "$SAPI/provisioning/data/peer-config" --data-urlencode "PeerId=$PEER" > "$LAB/run/client/wg0.conf"
grep -q "Endpoint = vpn.lab.test:51820" "$LAB/run/client/wg0.conf" || fail "client config has no DNS endpoint"
dc stop standalone

log "MIGRATION: import the standalone database into the fresh cluster database"
dc up -d orch-db
wait_for 120 "orchestrator database" dc exec -T orch-db mariadb -uwgportal -pdbpw -e 'select 1' wgportal
dc run --rm -T orch-wg -ha-import /import/sqlite.db 2>&1 | grep -E "imported|level=ERROR|panic" || true
if dc run --rm -T orch-wg -ha-import /import/sqlite.db >/dev/null 2>&1; then fail "second import was accepted"; fi
echo "ok: second import into a non-empty database refused"

log "starting orchestrator"
dc up -d orch-wg
wait_for 240 "orchestrator API" api "$API/ha/status"
api "$API/peer/by-interface/wg0" | grep -qF "$PEER" || fail "imported peer not found on the orchestrator"
echo "ok: imported interface and peer present"

log "joining nodes with a join token"
JOIN_TOKEN=$(api -X POST "$API/ha/join-tokens" --data '{"Uses":2,"ValidHours":1}' | json "d['Token']")
export JOIN_TOKEN
dc up -d a-db a-wg a-egress
wait_for 240 "node-a WireGuard up with imported client peer + probe peer" peers_are a-wg 2
dc up -d b-db b-wg b-egress
wait_for 240 "node-b WireGuard up with imported client peer + probe peer" peers_are b-wg 2
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API/ha/register" -H "Authorization: Bearer $JOIN_TOKEN"   -H 'Content-Type: application/json' --data '{"NodeId":"node-c","PublicIp":"10.99.0.13"}')
[ "$code" = 401 ] || fail "used up join token: expected 401, got $code"
echo "ok: used up join token rejected"

log "bootstrap: highest priority healthy node becomes active"
wait_for 90 "node-a active" is_active node-a
wait_for 30 "vpn.lab.test -> 10.99.0.11" dns_is 10.99.0.11

log "client connects with its unchanged pre-migration config"
dc exec -T client wg-quick up wg0
wait_for 30 "client traffic exits via node-a" egress_is 10.99.0.11

log "standby does not write statistics"
sleep 12
dc exec -T b-wg sh -c 'wget -qO- 127.0.0.1:8787/metrics 2>/dev/null | grep -c wireguard_peer_up || true'

log "FAILOVER: node-a gets blacklisted (unreachable from orchestrator and client)"
block wgp-ha-lab-orch-db-1 10.99.0.11
block wgp-ha-lab-client-1 10.99.0.11
start=$SECONDS
wait_for 90 "node-b promoted" is_active node-b
wait_for 30 "vpn.lab.test -> 10.99.0.12" dns_is 10.99.0.12
# client side re-resolve, as the systemd timer would run it (lab: 15s max age, every 5s)
until egress_is 10.99.0.12; do
  ((SECONDS - start < 180)) || fail "client did not follow the failover"
  dc exec -T client wg-reresolve.sh wg0 /etc/wireguard/wg0.conf 15
  sleep 5
done
echo "ok: client follows failover, traffic exits via node-b ($((SECONDS - start))s after blacklisting)"

log "changes made after failover reach all nodes"
api "$API/peer/prepare/wg0" | api -X POST "$API/peer/new" --data @- >/dev/null
wait_for 30 "new peer on node-b" peers_are b-wg 3
wait_for 30 "new peer on node-a" peers_are a-wg 3

log "no automatic fail-back, manual promote back to node-a"
unblock wgp-ha-lab-orch-db-1 10.99.0.11
unblock wgp-ha-lab-client-1 10.99.0.11
sleep 15
is_active node-b || fail "automatic fail-back happened"
echo "ok: node-b stays active"
api -X POST "$API/ha/promote/node-a" >/dev/null
wait_for 10 "node-a active" is_active node-a
wait_for 30 "vpn.lab.test -> 10.99.0.11" dns_is 10.99.0.11

log "orchestrator outage: tunnels keep working"
dc stop orch-wg orch-db
sleep 10
egress_is 10.99.0.12 || egress_is 10.99.0.11 || fail "client lost connectivity while the orchestrator is down"
echo "ok: client traffic flows while the orchestrator is down ($(client_egress))"
dc start orch-db orch-wg
wait_for 240 "orchestrator back" api "$API/ha/status"

log "ALL SCENARIOS PASSED"
api "$API/ha/status" | python3 -m json.tool
