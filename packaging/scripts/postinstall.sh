#!/bin/sh
# Runs after install and upgrade, for both .deb (configure) and .rpm (1 or 2).
set -e

if [ -d /run/systemd/system ]; then
  systemctl daemon-reload || true
  # Upgrades restart a running agent; a fresh install is left stopped until
  # it is configured. KillMode=process keeps a running package manager alive
  # if the agent upgraded itself.
  systemctl try-restart linux-ha-agent.service || true
fi

if ! systemctl is-enabled --quiet linux-ha-agent.service 2>/dev/null; then
  cat <<'MSG'
linux-ha-agent is installed. To start it:
  1. edit /etc/linux-ha-agent/config.yaml (at least mqtt.url and mqtt.username)
  2. put the MQTT password in /etc/linux-ha-agent/mqtt-password (chmod 600)
  3. systemctl enable --now linux-ha-agent
MSG
fi
