#!/usr/bin/env bash
# Create the guests the end-to-end test expects. Run as root on a disposable
# host with an initialised Incus.
set -euo pipefail

REGISTRY="public.ecr.aws/docker/library"
UBUNTU="${UBUNTU:-24.04}"
FEDORA="${FEDORA:-43}"
DEBIAN="${DEBIAN:-13}"
NESTED=(-c security.nesting=true -c security.syscalls.intercept.mknod=true -c security.syscalls.intercept.setxattr=true)

wait_for_network() {
  echo "waiting for network in $*"
  # shellcheck disable=SC2016 # expanded inside the guest
  incus exec "$@" -- sh -c \
    'for i in $(seq 120); do getent hosts deb.debian.org >/dev/null 2>&1 && exit 0; sleep 2; done; exit 1'
}

compose_file() {
  cat <<YAML
services:
  sleeper:
    image: ${REGISTRY}/busybox:latest
    command: [sleep, infinity]
    restart: unless-stopped
YAML
}

incus launch "images:ubuntu/${UBUNTU}" docker-web "${NESTED[@]}"
incus launch "images:fedora/${FEDORA}" podman-fed "${NESTED[@]}"
incus project create lab -c features.images=false -c features.profiles=false
incus launch "images:debian/${DEBIAN}" deb --project lab
incus launch images:alpine/edge alpine
incus init "images:ubuntu/${UBUNTU}" stopped

wait_for_network docker-web
wait_for_network podman-fed
wait_for_network deb --project lab
wait_for_network alpine

# Ubuntu with docker: a compose project whose "latest" image is really an
# older release, so the agent sees a newer image in the registry, plus a
# dangling image for the cleanup.
incus exec docker-web --env DEBIAN_FRONTEND=noninteractive -- sh -c \
  'apt-get update -q && apt-get install -y -q docker.io docker-compose-v2'
incus exec docker-web -- sh -c "
  docker pull -q ${REGISTRY}/busybox:1.36 &&
  docker tag ${REGISTRY}/busybox:1.36 ${REGISTRY}/busybox:latest &&
  docker pull -q ${REGISTRY}/alpine:3.19 && docker tag ${REGISTRY}/alpine:3.19 dangling:latest &&
  docker rmi ${REGISTRY}/alpine:3.19 &&
  docker pull -q ${REGISTRY}/alpine:3.20 && docker tag ${REGISTRY}/alpine:3.20 dangling:latest"
incus exec docker-web -- mkdir -p /root/app
compose_file | incus exec docker-web -- sh -c 'cat > /root/app/compose.yaml'
incus exec docker-web -- sh -c 'cd /root/app && docker compose up -d'

# Fedora with podman and podman-compose (vfs storage works in any container).
incus exec podman-fed -- dnf -y -q install podman podman-compose
incus exec podman-fed -- sh -c 'mkdir -p /etc/containers && printf "[storage]\ndriver = \"vfs\"\n" > /etc/containers/storage.conf'
incus exec podman-fed -- sh -c "
  podman pull -q ${REGISTRY}/busybox:1.36 &&
  podman tag ${REGISTRY}/busybox:1.36 ${REGISTRY}/busybox:latest"
incus exec podman-fed -- mkdir -p /root/app
compose_file | incus exec podman-fed -- sh -c 'cat > /root/app/compose.yaml'
incus exec podman-fed -- sh -c 'cd /root/app && podman-compose up -d'

incus list --all-projects
