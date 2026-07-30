#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2
  exit 1
fi

systemctl disable --now runonspark-manager.service 2>/dev/null || true
rm -f /etc/systemd/system/runonspark-manager.service
rm -f /usr/lib/runonspark-manager/runonspark-manager
rmdir /usr/lib/runonspark-manager 2>/dev/null || true
systemctl daemon-reload

echo "Manager removed. /var/lib/runonspark-manager and downloaded model data were preserved."
echo "Use the authenticated manager removal flow before uninstalling if model artifacts should be deleted."
