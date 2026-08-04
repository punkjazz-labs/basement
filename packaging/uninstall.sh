#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2
  exit 1
fi

# Model containers are detached from the manager service, so they survive the
# service stop below and would otherwise keep holding the GPU with no supported
# way left to stop them once the manager is gone. Both label namespaces are
# checked (repeated --filter label= values are ORed by Docker) so a container
# still carrying the pre-rename (spec 10) label is not left running.
if command -v docker >/dev/null 2>&1; then
  managed=$(docker ps --quiet --filter label=ai.basement.managed=true --filter label=ai.runonspark.managed=true 2>/dev/null || true)
  if [ -n "$managed" ]; then
    echo "Stopping managed model containers..."
    # shellcheck disable=SC2086
    docker stop --time 30 $managed >/dev/null || echo "warning: a managed model container could not be stopped; stop it manually with: docker ps --filter label=ai.basement.managed=true --filter label=ai.runonspark.managed=true" >&2
  fi
else
  echo "warning: docker is unavailable; any running managed model container was left running (docker ps --filter label=ai.basement.managed=true --filter label=ai.runonspark.managed=true)" >&2
fi

systemctl disable --now basement-updater.path basement-updater.service 2>/dev/null || true
systemctl disable --now basement.service 2>/dev/null || true
rm -f /etc/systemd/system/basement.service
rm -f /etc/systemd/system/basement-updater.service
rm -f /etc/systemd/system/basement-updater.path
rm -f /usr/lib/basement/basement
rm -f /usr/lib/basement/current
rm -rf /usr/lib/basement/versions
rm -rf /usr/lib/basement/updater
rmdir /usr/lib/basement 2>/dev/null || true
systemctl daemon-reload

echo "Manager removed. Managed model containers were stopped but not deleted."
echo "/var/lib/basement and downloaded model data were preserved."
echo "/var/lib/basement-updater and update receipts were preserved."
echo "Use the authenticated manager removal flow before uninstalling if model artifacts should be deleted."
