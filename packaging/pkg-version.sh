#!/bin/sh
# Turns a git version into a semver package version:
#   v1.2.3 -> 1.2.3, anything else (commits, dirty trees) -> 0.0.0-dev+<version>
v="${1#v}"
case "$v" in
  [0-9]*.[0-9]*.[0-9]*) echo "$v" ;;
  *) echo "0.0.0-dev+$(echo "$v" | tr -c 'A-Za-z0-9.\n' '.')" ;;
esac
