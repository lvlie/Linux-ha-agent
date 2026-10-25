#!/bin/sh
# Install or update linux-ha-agent.
#   sh install.sh [version]     (default: latest, e.g. v0.2.0)
# Where apt or dnf is available, adds the signed package repository on GitHub
# Pages and installs the package from it, so later versions come with the
# normal system updates. Otherwise installs the binary and systemd unit from
# a GitHub release, verifying the SHA-256 checksum and, when the GitHub CLI is
# installed, the signed build provenance.
set -eu

REPO="lvlie/linux-ha-agent"
REPO_URL="${LINUX_HA_AGENT_REPO_URL:-https://lvlie.github.io/Linux-ha-agent}"
TAG="${1:-latest}"
version="${TAG#v}"

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2
  exit 1
fi

if command -v apt-get >/dev/null 2>&1; then
  export DEBIAN_FRONTEND=noninteractive
  install -d -m 0755 /etc/apt/keyrings
  curl -fsSL -o /etc/apt/keyrings/linux-ha-agent.asc "$REPO_URL/linux-ha-agent.asc"
  cat >/etc/apt/sources.list.d/linux-ha-agent.sources <<EOF
Types: deb
URIs: $REPO_URL/deb/
Suites: ./
Signed-By: /etc/apt/keyrings/linux-ha-agent.asc
EOF
  apt-get update -q
  pkg=linux-ha-agent
  [ "$TAG" = latest ] || pkg="linux-ha-agent=$version-1"
  apt-get install -y --allow-downgrades "$pkg"
  linux-ha-agent -version
  exit 0
fi

if command -v dnf >/dev/null 2>&1; then
  cat >/etc/yum.repos.d/linux-ha-agent.repo <<EOF
[linux-ha-agent]
name=linux-ha-agent
baseurl=$REPO_URL/rpm/
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=$REPO_URL/linux-ha-agent.asc
EOF
  pkg=linux-ha-agent
  [ "$TAG" = latest ] || pkg="linux-ha-agent-$version-1"
  # -y also imports the repository key on first use.
  dnf -y install "$pkg"
  linux-ha-agent -version
  exit 0
fi

case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

if [ "$TAG" = latest ]; then
  # The latest release URL redirects to .../releases/tag/<tag>.
  TAG=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest")
  TAG="${TAG##*/}"
fi
base="https://github.com/$REPO/releases/download/$TAG"
asset="linux-ha-agent-linux-$arch"

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
linux-ha-agent -version
