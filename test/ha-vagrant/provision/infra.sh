#!/bin/bash
# Fake ipify (:8080) + fake reg.ru API (:8081) on .50, CoreDNS for lab.test and policy.test on .53.
# .57-.61 are extra targets for the routing scenario; the fakes listen on all addresses.
set -euo pipefail
LAB=/repo/test/ha-vagrant

# .53 (DNS) is a second address from lab-base.sh: clients route .50 (ipify) through the tunnel, DNS must stay
# reachable without it. systemd-resolved's stub only listens on 127.0.0.53, so CoreDNS can bind .53:53.
mkdir -p /srv/dns /opt/lab
install -m 755 "$LAB/.cache/fakes" /opt/lab/fakes
cat > /etc/systemd/system/lab-fakes.service <<EOF
[Unit]
Description=Lab fakes (ipify, reg.ru API)
After=network-online.target

[Service]
Environment=HOSTS_FILE=/srv/dns/hosts
ExecStart=/opt/lab/fakes
Restart=always

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now lab-fakes

cp "$LAB/Corefile" "$LAB/policy.hosts" /srv/dns/
docker rm -f coredns >/dev/null 2>&1 || true
docker run -d --name coredns --restart unless-stopped --network host -v /srv/dns:/dns:ro \
  coredns/coredns:1.11.3 -conf /dns/Corefile
