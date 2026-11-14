#!/bin/sh
# Re-resolve the DNS endpoint of WireGuard peers whose handshake is stale, so a client follows the
# HA DNS flip without a restart. Equivalent to wireguard-tools contrib/reresolve-dns.
#
# usage: wg-reresolve.sh <interface> [config file, default /etc/wireguard/<interface>.conf] [max handshake age, default 135s]
# Run it every 30s, e.g. with the systemd timer in this directory.
# Works without PersistentKeepalive: an idle tunnel has a stale handshake, so its endpoint is kept current.
set -eu

IFACE=$1
CONF=${2:-/etc/wireguard/$IFACE.conf}
MAX_AGE=${3:-135}
NOW=$(date +%s)

awk '
                              { sub(/\r$/, "") } # configs downloaded from wg-portal use CRLF
  /^[ \t]*\[/                { pk = ""; ep = "" }
  /^[ \t]*PublicKey[ \t]*=/   { sub(/^[^=]*=[ \t]*/, ""); pk = $0 }
  /^[ \t]*Endpoint[ \t]*=/    { sub(/^[^=]*=[ \t]*/, ""); ep = $0 }
  pk != "" && ep != ""        { print pk, ep; pk = ""; ep = "" }
' "$CONF" | while read -r pk ep; do
  hs=$(wg show "$IFACE" latest-handshakes | awk -v k="$pk" '$1 == k { print $2 }')
  [ -n "$hs" ] || continue
  if [ $((NOW - hs)) -gt "$MAX_AGE" ]; then
    wg set "$IFACE" peer "$pk" endpoint "$ep" # wg resolves the name again
  fi
done
