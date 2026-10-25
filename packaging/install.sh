#!/bin/sh
# Install or update linux-ha-agent from a GitHub release.
#   sh install.sh [version]     (default: latest, e.g. v0.2.0)
# Uses the .deb or .rpm package where apt or dnf is available, otherwise
# installs the binary and systemd unit. Verifies the SHA-256 checksum and,
# when the GitHub CLI is installed, the signed build provenance.
set -eu

REPO="lvlie/linux-ha-agent"
TAG="${1:-latest}"

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2
  exit 1
fi

case "$(uname -m)" in
  x86_64) arch=amd64 rpmarch=x86_64 ;;
  aarch64 | arm64) arch=arm64 rpmarch=aarch64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

if [ "$TAG" = latest ]; then
  # The latest release URL redirects to .../releases/tag/<tag>.
  TAG=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest")
  TAG="${TAG##*/}"
fi
version="${TAG#v}"
base="https://github.com/$REPO/releases/download/$TAG"

if command -v apt-get >/dev/null 2>&1; then
  asset="linux-ha-agent_${version}-1_${arch}.deb"
elif command -v dnf >/dev/null 2>&1; then
  asset="linux-ha-agent-${version}-1.${rpmarch}.rpm"
else
  asset="linux-ha-agent-linux-$arch"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "downloading $asset ($TAG)"
curl -fsSL -o "$tmp/$asset" "$base/$asset"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS"
(cd "$tmp" && grep " $asset\$" SHA256SUMS | sha256sum -c -)
# gh attestation needs GitHub CLI 2.49+ (distributions ship older ones, e.g.
# 2.45 on Ubuntu 24.04) and a login; a failed verification aborts.
if ! command -v gh >/dev/null 2>&1; then
  echo "note: install the GitHub CLI to also verify the build provenance"
elif ! gh attestation verify --help >/dev/null 2>&1; then
  echo "note: GitHub CLI $(gh --version | sed -n 's/^gh version \([^ ]*\).*/\1/p') cannot verify the build provenance (2.49+ needed), skipped"
elif ! gh auth status >/dev/null 2>&1; then
  echo "note: log in with 'gh auth login' to also verify the build provenance, skipped"
else
  gh attestation verify "$tmp/$asset" --repo "$REPO"
fi

case "$asset" in
  *.deb) DEBIAN_FRONTEND=noninteractive apt-get install -y "$tmp/$asset" ;;
  *.rpm) dnf -y install "$tmp/$asset" ;;
  *)
    install -m 0755 "$tmp/$asset" /usr/local/bin/linux-ha-agent
    install -d -m 0750 /etc/linux-ha-agent
    if [ ! -e /etc/linux-ha-agent/config.yaml ]; then
      curl -fsSL -o /etc/linux-ha-agent/config.yaml "$base/config.example.yaml"
      chmod 0640 /etc/linux-ha-agent/config.yaml
    fi
    curl -fsSL "$base/linux-ha-agent.service" |
      sed 's|/usr/bin/linux-ha-agent|/usr/local/bin/linux-ha-agent|g' >/etc/systemd/system/linux-ha-agent.service
    systemctl daemon-reload
    systemctl try-restart linux-ha-agent.service || true
    echo "edit /etc/linux-ha-agent/config.yaml, then: systemctl enable --now linux-ha-agent"
    ;;
esac
linux-ha-agent -version
