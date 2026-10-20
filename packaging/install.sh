#!/bin/sh
# Install or update incus-ha-agent from a GitHub release.
#   sh install.sh [version]     (default: latest)
# Verifies the SHA-256 checksum, and the build provenance when the GitHub CLI
# is available. Never overwrites an existing configuration.
set -eu

REPO="lvlie/incus-ha-agent"
VERSION="${1:-latest}"
BIN=/usr/local/bin/incus-ha-agent
CONF_DIR=/etc/incus-ha-agent
UNIT=/etc/systemd/system/incus-ha-agent.service

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2
  exit 1
fi

case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

if [ "$VERSION" = latest ]; then
  base="https://github.com/$REPO/releases/latest/download"
else
  base="https://github.com/$REPO/releases/download/$VERSION"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
asset="incus-ha-agent-linux-$arch"
curl -fsSL -o "$tmp/$asset" "$base/$asset"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS"
(cd "$tmp" && grep " $asset\$" SHA256SUMS | sha256sum -c -)

if command -v gh >/dev/null 2>&1; then
  gh attestation verify "$tmp/$asset" --repo "$REPO"
else
  echo "note: install the GitHub CLI to also verify the build provenance"
fi

install -m 0755 "$tmp/$asset" "$BIN"
install -d -m 0750 "$CONF_DIR"
if [ ! -e "$CONF_DIR/config.yaml" ]; then
  curl -fsSL -o "$CONF_DIR/config.yaml" "$base/config.example.yaml"
  chmod 0640 "$CONF_DIR/config.yaml"
  echo "wrote $CONF_DIR/config.yaml: edit it, then: systemctl enable --now incus-ha-agent"
fi
curl -fsSL -o "$UNIT" "$base/incus-ha-agent.service"
systemctl daemon-reload
if systemctl is-active --quiet incus-ha-agent; then
  systemctl restart incus-ha-agent
fi
"$BIN" -version
