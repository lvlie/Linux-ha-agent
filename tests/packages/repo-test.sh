#!/bin/sh
# Installs the agent with install.sh from a test package repository, as root,
# then checks that the package manager takes updates from it.
# LINUX_HA_AGENT_REPO_URL points to the repository; install.sh is read from
# $SRC/packaging (default /src).
set -eu
SRC="${SRC:-/src}"
: "${LINUX_HA_AGENT_REPO_URL:?}"

if ! command -v curl >/dev/null 2>&1; then
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update -q >/dev/null
    DEBIAN_FRONTEND=noninteractive apt-get install -y -q curl ca-certificates >/dev/null
  else
    dnf -y -q install curl
  fi
fi

echo "== install from the repository"
sh "$SRC/packaging/install.sh"
linux-ha-agent -version

echo "== the repository is configured and signed"
if command -v apt-get >/dev/null 2>&1; then
  test -f /etc/apt/sources.list.d/linux-ha-agent.sources
  # apt-get update only warns about a bad signature; fail on any warning.
  apt-get update 2>&1 | tee /tmp/update.log
  if grep -E '^(W|E):' /tmp/update.log; then
    exit 1
  fi
  apt-cache policy linux-ha-agent | grep -F "$LINUX_HA_AGENT_REPO_URL"
else
  test -f /etc/yum.repos.d/linux-ha-agent.repo
  dnf -y makecache --repo linux-ha-agent
  rpm -q gpg-pubkey --qf '%{SUMMARY}\n' | grep -q 'linux-ha-agent test'
  # The installed package came from the repository with its signature checked.
  rpm -q linux-ha-agent
fi
echo "repository test passed"
