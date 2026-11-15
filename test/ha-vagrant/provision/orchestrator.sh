#!/bin/bash
# Orchestrator from deploy/ha, then: interface wg0, client and exit peers, the DNS forwarder setting and a join
# token for the later VMs (served from /var/lib/lab-state).
set -euo pipefail
LAB=/repo/test/ha-vagrant
STATE=/var/lib/lab-state # outside /repo, which is replaced on every provisioning run
API=http://127.0.0.1:8888/api/v1
AUTH=admin@wgportal.local:lab-admin-api-token-123456
api() { curl -sf -u "$AUTH" -H 'Content-Type: application/json' "$@"; }

mkdir -p /opt/wg-portal "$STATE"
cd /opt/wg-portal
cp /repo/deploy/ha/docker-compose.ha-orchestrator.yml "$LAB/orchestrator.override.yml" .
rm -rf initdb && cp -r /repo/deploy/ha/initdb . && chmod 755 initdb/*.sh # shared folders lose the exec bit
cat > .env <<'EOF'
WG_PORTAL_IMAGE=wg-portal:ha-lab
PUBLIC_IP=192.168.77.10
DB_PASSWORD=lab-db-password
DB_ROOT_PASSWORD=lab-root-password
SST_PASSWORD=lab-sst-password
ENCRYPTION_PASSPHRASE=lab-encryption-passphrase
EXTERNAL_URL=http://192.168.77.10:8888
REGRU_USERNAME=lab
REGRU_PASSWORD=lab
REGRU_ZONE=lab.test
REGRU_SUBDOMAIN=vpn
UI_BIND=0.0.0.0:8888
EOF
docker compose -f docker-compose.ha-orchestrator.yml -f orchestrator.override.yml up -d

for _ in $(seq 120); do api "$API/ha/status" >/dev/null 2>&1 && break; sleep 3; done
api "$API/ha/status" >/dev/null

if ! api "$API/interface/all" | grep -q '"Identifier":"wg0"'; then
  api "$API/interface/prepare" | python3 -c '
import json,sys
i=json.load(sys.stdin)
i.update(Identifier="wg0", Mode="server", ListenPort=51820, Addresses=["10.11.12.1/24"],
         PeerDefNetwork=["10.11.12.0/24"], PeerDefEndpoint="vpn.lab.test:51820",
         # .56/29 (.56-.63): routing scenario targets on infra; must not contain the lab DNS .53
         PeerDefAllowedIPs=["10.11.12.0/24","192.168.77.50/32","192.168.77.56/29"], PeerDefPersistentKeepalive=25,
         # Docker sets the FORWARD policy to DROP, so forwarding has to be allowed explicitly
         PostUp="iptables -I FORWARD -i %i -j ACCEPT; iptables -I FORWARD -o %i -j ACCEPT; "
                "iptables -t nat -A POSTROUTING -s 10.11.12.0/24 ! -d 10.11.12.0/24 -j MASQUERADE",
         PostDown="iptables -D FORWARD -i %i -j ACCEPT; iptables -D FORWARD -o %i -j ACCEPT; "
                  "iptables -t nat -D POSTROUTING -s 10.11.12.0/24 ! -d 10.11.12.0/24 -j MASQUERADE")
print(json.dumps(i))' | api -X POST "$API/interface/new" --data @- >/dev/null
fi

for client in client-linux client-win exit-a exit-b; do
  [ -s "$STATE/$client.conf" ] && continue
  peer=$(api "$API/peer/prepare/wg0" | python3 -c "
import json,sys
p=json.load(sys.stdin); p['DisplayName']='$client'
# exit hosts route only the VPN subnet into the tunnel, their own traffic leaves through the lab NIC
if '$client'.startswith('exit-'): p['AllowedIPs']={'Value':['10.11.12.0/24'],'Overridable':False}
print(json.dumps(p))" |
    api -X POST "$API/peer/new" --data @- | python3 -c 'import json,sys; print(json.load(sys.stdin)["Identifier"])')
  api -G "$API/provisioning/data/peer-config" --data-urlencode "PeerId=$peer" > "$STATE/$client.conf"
done

# routing settings are v0 (UI API) only: session cookie + CSRF token; login starts a fresh session, so the
# token is fetched again after it
V0=http://127.0.0.1:8888/api/v0
jar=$(mktemp)
csrf() { curl -sf -b "$jar" -c "$jar" "$V0/csrf" | tr -d '"'; }
v0() { curl -sf -b "$jar" -c "$jar" -H 'Content-Type: application/json' -H "X-CSRF-TOKEN: $token" "$@"; }
token=$(csrf)
v0 -X POST "$V0/auth/login" --data '{"username":"admin@wgportal.local","password":"lab-admin-password-123"}' >/dev/null
token=$(csrf)
v0 -X PUT "$V0/routing/settings" --data '{"DnsForwarderEnabled":true,"DnsUpstreams":["192.168.77.53"]}' >/dev/null
rm -f "$jar"

api -X POST "$API/ha/join-tokens" --data '{"Uses":2,"ValidHours":4}' |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["Token"])' > "$STATE/join-token"
# later VMs fetch the join token and their client configs from here (lab only: no auth)
systemctl stop lab-state 2>/dev/null || true
systemd-run --unit lab-state python3 -m http.server 8099 --bind 192.168.77.10 --directory "$STATE"
echo "orchestrator ready, join token and client configs served on http://192.168.77.10:8099/"
