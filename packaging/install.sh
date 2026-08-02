#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2
  exit 1
fi
if [ "$#" -ne 2 ] || [ ! -f "$1" ] || [ ! -f "$2" ]; then
  echo "usage: $0 /path/to/basement /path/to/basement.sha256" >&2
  exit 2
fi

if ! getent group docker >/dev/null 2>&1; then
  echo "Docker group is missing; install Docker before basement" >&2
  exit 1
fi
expected=$(awk 'NR == 1 { print $1 }' "$2")
actual=$(sha256sum "$1" | awk '{ print $1 }')
if ! printf '%s\n' "$expected" | grep -Eq '^[0-9a-fA-F]{64}$' || [ "$actual" != "$expected" ]; then
  echo "binary checksum verification failed" >&2
  exit 1
fi
unit_source="$(dirname "$0")/systemd/basement.service"
if [ ! -f "$unit_source" ]; then
  echo "systemd unit not found at $unit_source; keep install.sh next to its systemd/ directory from the release package" >&2
  exit 1
fi

if ! getent group basement >/dev/null 2>&1; then groupadd --system basement; fi
if ! getent passwd basement >/dev/null 2>&1; then
  useradd --system --gid basement --home-dir /var/lib/basement --shell /usr/sbin/nologin basement
fi
usermod -a -G docker basement
install -d -m 0755 /usr/lib/basement
install -m 0755 "$1" /usr/lib/basement/basement
install -d -o basement -g basement -m 0750 /var/lib/basement
install -m 0644 "$unit_source" /etc/systemd/system/basement.service

# The console binds loopback unless the operator deliberately chooses an
# interface here (or pre-seeds BASEMENT_LISTEN for unattended installs).
listen="${BASEMENT_LISTEN:-}"
if [ -z "$listen" ] && [ -t 0 ]; then
  tailscale_ip=$(tailscale ip -4 2>/dev/null | head -n1 || true)
  lan_ip=$(hostname -I 2>/dev/null | awk '{ print $1 }' || true)
  echo
  echo "Where should the basement console be reachable?"
  echo "  1) This machine only (127.0.0.1) [default]"
  [ -n "$tailscale_ip" ] && echo "  2) Your Tailscale network ($tailscale_ip)"
  [ -n "$lan_ip" ] && echo "  3) Your local network ($lan_ip)"
  printf "Choice [1]: "
  read -r choice || choice=1
  case "$choice" in
    2) [ -n "$tailscale_ip" ] && listen="$tailscale_ip:7070" ;;
    3) [ -n "$lan_ip" ] && listen="$lan_ip:7070" ;;
  esac
fi
if [ -n "$listen" ]; then
  install -d -m 0755 /etc/systemd/system/basement.service.d
  printf '[Service]\nExecStart=\nExecStart=/usr/lib/basement/basement --data-dir /var/lib/basement --listen %s\n' "$listen" \
    > /etc/systemd/system/basement.service.d/listen.conf
fi

systemctl daemon-reload
systemctl enable --now basement.service

token_file=/var/lib/basement/pairing-token
tries=0
while [ ! -s "$token_file" ] && [ "$tries" -lt 20 ]; do
  tries=$((tries + 1))
  sleep 0.5
done

port=7070
case "$listen" in *:*) port=${listen##*:} ;; esac
host_short=$(hostname -s 2>/dev/null || hostname)
case "$listen" in
  "" | 127.0.0.1:*) console_url="http://127.0.0.1:${port}" ;;
  *) console_url="http://${listen}" ;;
esac

echo
echo "=================================================================="
echo "  basement is running."
echo
echo "  Open the console:  $console_url"
if [ -z "$listen" ]; then
  echo "  (loopback only — from another device use an SSH tunnel, or rerun"
  echo "   install.sh and pick a network interface)"
else
  case "$listen" in
    "$lan_ip":*) echo "  Also try:          http://${host_short}.local:${port}" ;;
  esac
fi
if [ -s "$token_file" ]; then
  echo
  echo "  Pairing token:     $(cat "$token_file")"
else
  echo
  echo "  Pairing token appears shortly at: $token_file"
fi
echo
echo "  Re-print this card anytime with:"
echo "    /usr/lib/basement/basement pairing-url"
if command -v qrencode >/dev/null 2>&1; then
  echo
  qrencode -t ANSIUTF8 "$console_url" || true
elif [ -n "$listen" ]; then
  echo
  echo "  (install 'qrencode' to also get a scannable QR code here)"
fi
echo "=================================================================="
