#!/usr/bin/env bash
# Create the guests the end-to-end test expects. Run as root on a disposable
# host with an initialised Incus.
set -euo pipefail

# Google's cache of Docker Hub images, more forgiving than pulling directly.
REGISTRY="mirror.gcr.io/library"
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

# retry <guest> <engine> pull <image>: registries hiccup now and then.
pull() {
  local guest=$1 engine=$2 image=$3
  for i in 1 2 3 4 5; do
    incus exec "$guest" -- "$engine" pull -q "$image" && return 0
    echo "pull of $image failed (attempt $i), retrying"
    sleep $((i * 5))
  done
  return 1
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
pull docker-web docker "${REGISTRY}/busybox:1.36"
pull docker-web docker "${REGISTRY}/alpine:3.19"
pull docker-web docker "${REGISTRY}/alpine:3.20"
incus exec docker-web -- sh -c "
  docker tag ${REGISTRY}/busybox:1.36 ${REGISTRY}/busybox:latest &&
  docker tag ${REGISTRY}/alpine:3.19 dangling:latest &&
  docker rmi ${REGISTRY}/alpine:3.19 &&
  docker tag ${REGISTRY}/alpine:3.20 dangling:latest"
incus exec docker-web -- mkdir -p /root/app
compose_file | incus exec docker-web -- sh -c 'cat > /root/app/compose.yaml'
incus exec docker-web -- sh -c 'cd /root/app && docker compose up -d'

# Fedora with podman and podman-compose (vfs storage works in any container).
incus exec podman-fed -- dnf -y -q install podman podman-compose
incus exec podman-fed -- sh -c 'mkdir -p /etc/containers && printf "[storage]\ndriver = \"vfs\"\n" > /etc/containers/storage.conf'
pull podman-fed podman "${REGISTRY}/busybox:1.36"
incus exec podman-fed -- podman tag "${REGISTRY}/busybox:1.36" "${REGISTRY}/busybox:latest"
incus exec podman-fed -- mkdir -p /root/app
compose_file | incus exec podman-fed -- sh -c 'cat > /root/app/compose.yaml'
incus exec podman-fed -- sh -c 'cd /root/app && podman-compose up -d'

# OCI application containers (Incus 6.3 and later; OCI=1): one launched
# directly from the registry and one managed by incus-compose.
if [ "${OCI:-0}" = 1 ]; then
  incus remote add gcr https://mirror.gcr.io --protocol=oci --public
  for i in 1 2 3 4 5; do
    incus launch gcr:library/busybox:1.36 oci-app -c oci.entrypoint="sleep 2147483647" && break
    echo "launch of the OCI container failed (attempt $i), retrying"
    incus delete --force oci-app 2>/dev/null || true
    sleep $((i * 5))
  done
  # incus-compose's image cache needs Incus listening on the network.
  incus config set core.https_address=127.0.0.1:8443
  mkdir -p /root/icapp
  cat >/root/icapp/compose.yaml <<YAML
name: icapp
services:
  web:
    image: ${REGISTRY}/busybox:1.36
    command: [sleep, "2147483647"]
YAML
  (cd /root/icapp && incus-compose up --detach)
fi

incus list --all-projects
