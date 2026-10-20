#!/bin/sh
# Stop the agent on removal, not on upgrade.
#   .deb: "remove" on removal, "upgrade" on upgrade
#   .rpm: 0 on removal, 1 on upgrade
set -e

case "$1" in
  remove | 0)
    if [ -d /run/systemd/system ]; then
      systemctl disable --now linux-ha-agent.service || true
    fi
    ;;
esac
