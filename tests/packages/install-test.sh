#!/bin/sh
# Installs, reinstalls and removes the package on a clean system, as root.
# FORMAT=deb|rpm; packages are read from $SRC/dist (default /src/dist).
set -eu
SRC="${SRC:-/src}"
CONF=/etc/linux-ha-agent/config.yaml

install_pkg() {
  case "$FORMAT" in
    deb)
      export DEBIAN_FRONTEND=noninteractive
      apt-get update -q >/dev/null
      apt-get install -y -q "$@" "$SRC"/dist/linux-ha-agent_*_amd64.deb
      ;;
    rpm) dnf -y -q "${1:-install}" "$SRC"/dist/linux-ha-agent-*.x86_64.rpm ;;
  esac
}

echo "== install"
install_pkg
linux-ha-agent -version
linux-ha-agent -config "$CONF" -check-config
test -f /usr/lib/systemd/system/linux-ha-agent.service
test "$(stat -c %a "$CONF")" = 640
test "$(stat -c %a /etc/linux-ha-agent)" = 750
test "$(stat -c %a /var/lib/linux-ha-agent)" = 700

echo "== reinstall keeps a changed configuration"
echo "# local change" >>"$CONF"
case "$FORMAT" in
  deb) install_pkg --reinstall ;;
  rpm) install_pkg reinstall ;;
esac
grep -q "# local change" "$CONF"

echo "== remove"
case "$FORMAT" in
  deb)
    apt-get remove -y -q linux-ha-agent
    test ! -e /usr/bin/linux-ha-agent
    test -e "$CONF" # kept until purge
    apt-get purge -y -q linux-ha-agent
    test ! -e "$CONF"
    ;;
  rpm)
    dnf -y -q remove linux-ha-agent
    test ! -e /usr/bin/linux-ha-agent
    test -e "$CONF.rpmsave" # changed configuration is saved
    ;;
esac
echo "package test passed"
