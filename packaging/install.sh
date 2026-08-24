#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2
  exit 1
fi
if [ "$#" -ne 4 ] || [ ! -f "$1" ] || [ ! -f "$2" ] || [ ! -f "$3" ] || [ ! -f "$4" ]; then
  echo "usage: $0 /path/to/basement /path/to/basement.sha256 /path/to/basement-updater /path/to/basement-updater.sha256" >&2
  exit 2
fi

if ! getent group docker >/dev/null 2>&1; then
  echo "Docker group is missing; install Docker before basement" >&2
  exit 1
fi
verify_checksum() {
  checksum_file=$1
  payload_file=$2
  expected=$(awk 'NR == 1 { print $1 }' "$checksum_file")
  actual=$(sha256sum "$payload_file" | awk '{ print $1 }')
  if ! printf '%s\n' "$expected" | grep -Eq '^[0-9a-fA-F]{64}$' || [ "$actual" != "$expected" ]; then
    echo "binary checksum verification failed for $payload_file" >&2
    exit 1
  fi
}

verify_checksum "$2" "$1"
verify_checksum "$4" "$3"

manager_version=$("$1" version 2>/dev/null || true)
if ! printf '%s\n' "$manager_version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
  echo "manager binary does not report a stable vMAJOR.MINOR.PATCH version" >&2
  exit 1
fi

unit_dir="$(dirname "$0")/systemd"
for unit_name in basement.service basement-updater.service basement-updater.path; do
  if [ ! -f "$unit_dir/$unit_name" ]; then
    echo "systemd unit not found at $unit_dir/$unit_name; keep install.sh next to its systemd/ directory from the release package" >&2
    exit 1
  fi
done
sudoers_source="$(dirname "$0")/sudoers/basement-power"
if [ ! -f "$sudoers_source" ]; then
  echo "GPU power grant not found at $sudoers_source; keep install.sh next to its sudoers/ directory from the release package" >&2
  exit 1
fi

if ! getent group basement >/dev/null 2>&1; then groupadd --system basement; fi
if ! getent passwd basement >/dev/null 2>&1; then
  useradd --system --gid basement --home-dir /var/lib/basement --shell /usr/sbin/nologin basement
fi
usermod -a -G docker basement
install -d -m 0755 /usr/lib/basement
install -d -m 0755 /usr/lib/basement/versions /usr/lib/basement/updater
install -d -o basement -g basement -m 0750 /var/lib/basement
install -d -o basement -g basement -m 0750 /var/lib/basement/updates
install -d -o basement -g basement -m 0750 /var/lib/basement/updates/staging
install -d -o basement -g basement -m 0750 /var/lib/basement/updates/staging/pending
install -d -o basement -g basement -m 0750 /var/lib/basement/updates/staging/partial
install -d -o root -g root -m 0755 /var/lib/basement-updater

# Stop only the privileged update trigger while its fixed helper and units are
# refreshed. The unprivileged manager keeps running until the new slot is ready.
systemctl disable --now basement-updater.path >/dev/null 2>&1 || true
systemctl stop basement-updater.service >/dev/null 2>&1 || true

flat_binary=/usr/lib/basement/basement
if [ -f "$flat_binary" ] && [ ! -L "$flat_binary" ]; then
  old_digest=$(sha256sum "$flat_binary" | awk '{ print $1 }')
  old_version=$("$flat_binary" version 2>/dev/null || true)
  if printf '%s\n' "$old_version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
    old_slot=$old_version
  else
    old_slot=bootstrap-$old_digest
  fi
  if [ ! -e "/usr/lib/basement/versions/$old_slot/basement" ]; then
    install -d -m 0755 "/usr/lib/basement/versions/$old_slot"
    install -m 0755 "$flat_binary" "/usr/lib/basement/versions/$old_slot/basement"
  fi
fi

target_slot="/usr/lib/basement/versions/$manager_version"
slot_tmp="/usr/lib/basement/versions/.$manager_version.install.$$"
rm -rf "$slot_tmp"
install -d -m 0755 "$slot_tmp"
install -m 0755 "$1" "$slot_tmp/basement"
if [ -d "$target_slot" ] && [ ! -f "$target_slot/basement" ]; then
  echo "version slot $manager_version is incomplete" >&2
  rm -rf "$slot_tmp"
  exit 1
elif [ -e "$target_slot/basement" ]; then
  target_digest=$(sha256sum "$target_slot/basement" | awk '{ print $1 }')
  incoming_digest=$(sha256sum "$slot_tmp/basement" | awk '{ print $1 }')
  if [ "$target_digest" != "$incoming_digest" ]; then
    echo "version slot $manager_version already exists with different bytes" >&2
    rm -rf "$slot_tmp"
    exit 1
  fi
  rm -rf "$slot_tmp"
elif [ ! -e "$target_slot" ]; then
  mv "$slot_tmp" "$target_slot"
else
  echo "version slot $manager_version is not a directory" >&2
  rm -rf "$slot_tmp"
  exit 1
fi

current_tmp="/usr/lib/basement/.current.$$"
rm -f "$current_tmp"
ln -s "versions/$manager_version" "$current_tmp"
mv -Tf "$current_tmp" /usr/lib/basement/current
rm -f "$flat_binary"
ln -s current/basement "$flat_binary"

install -m 0755 "$3" /usr/lib/basement/updater/basement-updater
install -m 0644 "$unit_dir/basement.service" /etc/systemd/system/basement.service
install -m 0644 "$unit_dir/basement-updater.service" /etc/systemd/system/basement-updater.service
install -m 0644 "$unit_dir/basement-updater.path" /etc/systemd/system/basement-updater.path

# The GPU clock belongs to root and the manager is not root, so the power switch
# runs its two commands through sudo. This is the whole grant: two command
# lines, no password, nothing else.
#
# The file is checked before it carries its real name. A broken file in
# /etc/sudoers.d makes every sudo on the machine fail, including the one that
# would repair it, so the bytes go to a temporary name in the same directory,
# pass visudo, and only then move into place. sudo ignores a name in that
# directory that carries a dot, so debris from an interrupted run is inert.
# Keep in sync with installSudoersScript in internal/setup/install.go.
if command -v visudo >/dev/null 2>&1; then
  install -d -o root -g root -m 0755 /etc/sudoers.d
  sudoers_tmp=$(mktemp /etc/sudoers.d/.basement-power.XXXXXX)
  trap 'rm -f "$sudoers_tmp"' EXIT
  install -o root -g root -m 0440 "$sudoers_source" "$sudoers_tmp"
  visudo -cf "$sudoers_tmp" >/dev/null
  mv -f "$sudoers_tmp" /etc/sudoers.d/basement-power
  trap - EXIT
else
  echo "this machine has no visudo, so the GPU power grant was not installed" >&2
fi

# The console binds loopback unless the operator deliberately chooses an
# interface here (or pre-seeds BASEMENT_LISTEN for unattended installs).
listen="${BASEMENT_LISTEN:-}"
tailscale_ip=""
lan_ip=""
if [ -z "$listen" ] && [ -t 0 ]; then
  tailscale_ip=$(tailscale ip -4 2>/dev/null | head -n1 || true)
  # The default route's source address, not the first `hostname -I` field: a
  # cluster port with link but no DHCP puts a self-assigned 169.254 address
  # first. Keep in sync with resolveListen in internal/setup/install.go.
  lan_ip=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.*src \([0-9.]*\).*/\1/p' | head -n1 || true)
  if [ -z "$lan_ip" ]; then
    lan_ip=$(hostname -I 2>/dev/null | tr ' ' '\n' | grep -v '^169\.254\.' | grep . | head -n1 || true)
  fi
  echo
  echo "Where should the basement console be reachable?"
  echo "  1) This machine only (127.0.0.1) [default]"
  [ -n "$tailscale_ip" ] && echo "  2) Your Tailscale network ($tailscale_ip)"
  [ -n "$lan_ip" ] && echo "  3) Your local network ($lan_ip)"
  [ -n "$lan_ip" ] && [ -n "$tailscale_ip" ] && echo "  4) Your local network and Tailscale"
  printf "Choice [1]: "
  read -r choice || choice=1
  case "$choice" in
    2) [ -n "$tailscale_ip" ] && listen="$tailscale_ip:7070" ;;
    3) [ -n "$lan_ip" ] && listen="$lan_ip:7070" ;;
    # Both at once. The console binds every address in the list, local
    # network first: that one is the primary address.
    4) [ -n "$lan_ip" ] && [ -n "$tailscale_ip" ] && listen="$lan_ip:7070,$tailscale_ip:7070" ;;
  esac
fi
if [ -n "$listen" ]; then
  install -d -m 0755 /etc/systemd/system/basement.service.d
  printf '[Service]\nExecStart=\nExecStart=/usr/lib/basement/basement --data-dir /var/lib/basement --listen %s\n' "$listen" \
    > /etc/systemd/system/basement.service.d/listen.conf
else
  # Nobody chose an interface on this run, which is what happens on every
  # unattended rerun and on the one-time manual upgrade to an updater-capable
  # version. An existing drop-in still governs where the console binds, so read
  # it back rather than assuming loopback. Without this the card below tells a
  # machine that has been on the LAN for months that it is loopback only, hands
  # out a 127.0.0.1 URL that does not work from the owner's laptop, and advises
  # an SSH tunnel nobody needs. That lands on every existing user exactly once,
  # during the upgrade that is already asking them to trust a new mechanism.
  existing_listen=$(sed -n 's|^ExecStart=.*--listen \([^ ]*\).*|\1|p' \
    /etc/systemd/system/basement.service.d/listen.conf 2>/dev/null | tail -n1 || true)
  case "$existing_listen" in
    *:*) listen="$existing_listen" ;;
  esac
fi

systemctl daemon-reload
systemctl enable basement.service basement-updater.service basement-updater.path
# Restart rather than `enable --now`. That form starts a stopped service but
# does nothing to one already running, so rerunning this installer to upgrade
# put the new binary on disk, left the old one running, and still printed
# "basement is running" below. Restarting unconditionally is what makes a
# rerun an actual upgrade. Model containers are detached from this service and
# keep serving across the restart.
systemctl restart basement.service
systemctl start basement-updater.service
systemctl start basement-updater.path

if ! systemctl is-active --quiet basement.service; then
  echo "basement.service did not start" >&2
  exit 1
fi
if ! systemctl is-active --quiet basement-updater.path; then
  echo "basement-updater.path did not start" >&2
  exit 1
fi
if [ "$(readlink /usr/lib/basement/current)" != "versions/$manager_version" ] || [ "$(readlink "$flat_binary")" != "current/basement" ]; then
  echo "manager version slot links were not installed correctly" >&2
  exit 1
fi

# --listen takes one address or a comma separated list, and a rerun reads
# whatever the drop-in already holds. The card, the health check and the QR
# code all follow the first address: every address serves the same console,
# and the first one is the primary.
primary_listen=${listen%%,*}
port=7070
case "$primary_listen" in *:*) port=${primary_listen##*:} ;; esac
host_short=$(hostname -s 2>/dev/null || hostname)
case "$primary_listen" in
  "" | 127.0.0.1:*) console_url="http://127.0.0.1:${port}" ;;
  *) console_url="http://${primary_listen}" ;;
esac

# An active systemd unit is not yet a usable installation. Require the exact
# configured HTTP endpoint and the pairing token before printing success.
token_file=/var/lib/basement/pairing-token
tries=0
while [ "$tries" -lt 90 ]; do
  if [ -s "$token_file" ] && curl -fsS --max-time 2 -o /dev/null "$console_url/healthz" 2>/dev/null; then
    break
  fi
  tries=$((tries + 1))
  sleep 1
done
if ! curl -fsS --max-time 2 -o /dev/null "$console_url/healthz"; then
  echo "basement.service started, but $console_url/healthz did not answer" >&2
  exit 1
fi
if [ ! -s "$token_file" ]; then
  echo "basement.service started, but no pairing token appeared at $token_file" >&2
  exit 1
fi

echo
echo "=================================================================="
echo "  basement is running."
echo
echo "  Open the console:  $console_url"
# Every other bound address, one per line, under the same heading.
extra_listen=${listen#"$primary_listen"}
extra_listen=${extra_listen#,}
while [ -n "$extra_listen" ]; do
  next=${extra_listen%%,*}
  echo "                     http://${next}"
  case "$extra_listen" in
    *,*) extra_listen=${extra_listen#*,} ;;
    *) extra_listen="" ;;
  esac
done
if [ -z "$listen" ]; then
  echo "  (loopback only. From another device use an SSH tunnel, or rerun"
  echo "   install.sh and pick a network interface)"
else
  case "$primary_listen" in
    "$lan_ip":*) echo "  Also try:          http://${host_short}.local:${port}" ;;
  esac
fi
echo
echo "  Pairing token:     $(cat "$token_file")"
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
