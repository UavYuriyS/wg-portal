#!/bin/bash
# Docker + the locally built wg-portal image (run.ps1 exports it to .cache/wg-portal.tar.gz).
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

if ! command -v docker >/dev/null; then
  apt-get update -q
  apt-get install -y -q docker.io docker-compose-v2 python3 curl wireguard-tools
fi
sysctl -q -w net.ipv4.ip_forward=1
echo net.ipv4.ip_forward=1 > /etc/sysctl.d/90-lab.conf

image=/repo/test/ha-vagrant/.cache/wg-portal.tar.gz
if [ -f "$image" ]; then
  gunzip -c "$image" | docker load -q
fi
