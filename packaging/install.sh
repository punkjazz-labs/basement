#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2
  exit 1
fi
if [ "$#" -ne 2 ] || [ ! -f "$1" ] || [ ! -f "$2" ]; then
  echo "usage: $0 /path/to/runonspark-manager /path/to/runonspark-manager.sha256" >&2
  exit 2
fi

if ! getent group docker >/dev/null 2>&1; then
  echo "Docker group is missing; install Docker before RunOnSpark Manager" >&2
  exit 1
fi
expected=$(awk 'NR == 1 { print $1 }' "$2")
actual=$(sha256sum "$1" | awk '{ print $1 }')
if ! printf '%s\n' "$expected" | grep -Eq '^[0-9a-fA-F]{64}$' || [ "$actual" != "$expected" ]; then
  echo "binary checksum verification failed" >&2
  exit 1
fi
if ! getent group runonspark >/dev/null 2>&1; then groupadd --system runonspark; fi
if ! getent passwd runonspark >/dev/null 2>&1; then
  useradd --system --gid runonspark --home-dir /var/lib/runonspark-manager --shell /usr/sbin/nologin runonspark
fi
usermod -a -G docker runonspark
install -d -m 0755 /usr/lib/runonspark-manager
install -m 0755 "$1" /usr/lib/runonspark-manager/runonspark-manager
install -d -o runonspark -g runonspark -m 0750 /var/lib/runonspark-manager
install -m 0644 "$(dirname "$0")/systemd/runonspark-manager.service" /etc/systemd/system/runonspark-manager.service
systemctl daemon-reload
systemctl enable runonspark-manager.service

echo "Installed but not started. Review the listen address in /etc/systemd/system/runonspark-manager.service, then run:"
echo "  systemctl start runonspark-manager"
echo "The pairing token will be created at /var/lib/runonspark-manager/pairing-token."
