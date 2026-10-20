# linux-ha-agent

[![CI](https://github.com/lvlie/linux-ha-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/lvlie/linux-ha-agent/actions/workflows/ci.yml)
[![End-to-end](https://github.com/lvlie/linux-ha-agent/actions/workflows/e2e.yml/badge.svg)](https://github.com/lvlie/linux-ha-agent/actions/workflows/e2e.yml)

A small agent that brings your Linux machines into Home Assistant over MQTT:
pending updates, reboot status, Docker and Podman containers, and controls to
upgrade, restart and roll back. On an [Incus](https://linuxcontainers.org/incus/)
or [Proxmox VE](https://www.proxmox.com/en/proxmox-virtual-environment) host it
also manages **every guest through the platform itself** (the Incus API, or
`pct`/`qm` on Proxmox), so containers and VMs need no agent, no SSH and no
credentials of their own.

Inspired by [LNXlink](https://github.com/bkbilly/lnxlink), written in Go as a
single static binary.

```text
Incus host:    linux-ha-agent ── Incus API (exec, snapshots, events) ──► containers, VMs
Proxmox node:  linux-ha-agent ── pct exec / QEMU guest agent, pvesh ──► LXC containers, VMs
                   │
                   └── MQTT (TLS, one user per host, ACL) ──► Mosquitto ──► Home Assistant ──► HA MCP / Claude
Other machines: linux-ha-agent (manages only itself)
```

## What you get in Home Assistant

Every machine and every guest becomes a device. Guests are linked to their
host (`via_device`).

| Entity | |
| --- | --- |
| **System updates** (update entity) | Pending packages; *Install* upgrades them. Shows up under Settings → Updates |
| **Container images** (update entity) | Images with a newer version in their registry; *Install* pulls them and recreates the compose projects |
| Pending updates / Security updates | Counts, with the package list as attributes |
| Reboot required | From `/var/run/reboot-required`, `dnf needs-restarting` or the NixOS booted system |
| Containers running / Container image updates | Per engine, with compose projects as attributes |
| Upgrade running, Last upgrade, Last upgrade result | Result has every step as attributes |
| Buttons | Check for updates, Upgrade, Reboot/Restart, Restore pre-upgrade snapshot (guests) |
| Host only | Upgrade host and guests, Automatic upgrades switch, Next scheduled upgrade, Managed guests |

Everything is exposed to Assist and so to the Home Assistant MCP server, so
Claude or any other assistant can answer "what needs updating?" or press
*Upgrade* with your approval.

## Supported systems

| | Packages | Notes |
| --- | --- | --- |
| Ubuntu, Debian and derivatives | apt | security updates detected from the `-security` pockets |
| Fedora, RHEL, Rocky, Alma, CentOS Stream | dnf (dnf4 and dnf5) | security updates from advisories |
| NixOS | `nixos-rebuild` | channels (`--upgrade`) or a flake (`--refresh`); garbage collection as cleanup |
| Others (e.g. Alpine) | – | listed with OS and containers; packages are skipped, not failed |

Virtualisation platforms (both can be used on one machine; detected automatically):

| | Guests | How commands run inside | Snapshots | Changes noticed by |
| --- | --- | --- | --- | --- |
| Incus | containers and VMs, all projects | Incus API exec (like `incus exec`) | Incus API, with native expiry | Incus event stream |
| Proxmox VE | LXC containers and QEMU VMs on the local node (templates skipped) | `pct exec`; `qm guest exec` through the QEMU guest agent | `pct`/`qm snapshot`; the agent removes its own expired snapshots | polling `pvesh` (every minute) |

Containers, on hosts and inside guests:

- **Docker** and **Podman** (a `docker` command that is really podman is detected).
- Compose projects are found from the labels compose puts on containers, so
  they are found wherever the files live. Upgrading runs `compose pull` +
  `compose up -d --remove-orphans` and only reports a change when an image
  actually changed. `podman compose` and `podman-compose` both work.
- Podman containers labelled `io.containers.autoupdate` are updated with
  `podman auto-update`.
- New images are detected by comparing local image digests with the registry
  (HEAD requests, which do not count against Docker Hub limits). Containers
  pinned by digest are left alone; standalone containers are reported but not
  recreated.

## How upgrades work

For a guest, in order:

1. **Snapshot** through the platform (`ha-agent-<time>`, removed after a week).
   If the snapshot fails, nothing is upgraded.
2. **Packages**: refresh and upgrade non-interactively (keeping existing config
   files), then remove unused packages and caches.
3. **Containers**: pull and recreate compose projects, podman auto-update, prune
   dangling images.
4. **Restart** the guest if the upgrade requires it (configurable).

*Upgrade host and guests* (button or schedule) upgrades the host first, then
every running guest one by one, then reboots the host only if it is needed
**and** `upgrade.reboot_host` is enabled. Only one operation runs at a time;
stopped guests are skipped. Lifecycle events (Incus) or polling (Proxmox) keep
the guest list current and trigger a check when a guest starts.

### Proxmox VE notes

- Install the agent on **every node**; each manages the guests running on it.
  Guests that migrate move to the other node's agent automatically.
- The node itself is Debian: it is upgraded with `apt-get dist-upgrade`, as
  Proxmox requires. Without a subscription, switch to the
  `pve-no-subscription` repository, or the enterprise repository makes every
  check fail with 401.
- Debian does not create `/var/run/reboot-required`; a pending reboot is
  detected by comparing the newest installed kernel with the running one.
- **VMs** need the QEMU guest agent: enable it in the VM options and install
  `qemu-guest-agent` inside. VMs without it (and non-Linux VMs) are listed
  with their state, but are not checked or upgraded.
- **Snapshots** need storage that supports them (ZFS, LVM-thin, Ceph, qcow2).
  On other storage the snapshot fails and the upgrade does not run; set
  `guests.snapshot_before_upgrade: false` for those guests' hosts if you
  accept upgrading without one.

## Install

Every [release](https://github.com/lvlie/linux-ha-agent/releases) has `.deb`
and `.rpm` packages and plain binaries for amd64 and arm64, with SHA-256
checksums and signed build provenance. The binary is static: it runs on any
Linux distribution without dependencies.

### Debian, Ubuntu, Proxmox VE (.deb)

```sh
curl -fsSLO https://github.com/lvlie/linux-ha-agent/releases/download/v0.1.0/linux-ha-agent_0.1.0-1_amd64.deb
sudo apt install ./linux-ha-agent_0.1.0-1_amd64.deb
```

### Fedora, RHEL, Rocky, Alma (.rpm)

```sh
sudo dnf install https://github.com/lvlie/linux-ha-agent/releases/download/v0.1.0/linux-ha-agent-0.1.0-1.x86_64.rpm
```

The packages install:

| | |
| --- | --- |
| `/usr/bin/linux-ha-agent` | the agent |
| `/etc/linux-ha-agent/config.yaml` | configuration (kept on upgrades and removal) |
| `/usr/lib/systemd/system/linux-ha-agent.service` | systemd unit, not started until you enable it |
| `/usr/share/doc/linux-ha-agent/` | README, example configuration, Mosquitto ACL |

Upgrading the package restarts a running agent. Removing it stops the
service; `apt purge` also removes the configuration.

### Install script

Picks the right package (or the plain binary on other distributions) for
the latest or a given release and verifies it:

```sh
curl -fsSLO https://github.com/lvlie/linux-ha-agent/releases/latest/download/install.sh
less install.sh        # read it first
sudo sh install.sh     # or: sudo sh install.sh v0.1.0
```

### With Go

With Go installed (1.26.7 or newer; older Go versions download it
automatically):

```sh
go install github.com/lvlie/linux-ha-agent/cmd/linux-ha-agent@latest
sudo install -m 0755 "$(go env GOPATH)/bin/linux-ha-agent" /usr/local/bin/
```

Then add the [systemd unit](packaging/systemd/linux-ha-agent.service) (change
`/usr/bin` to `/usr/local/bin`) and a configuration.

### After installing

```sh
sudoedit /etc/linux-ha-agent/config.yaml
echo 'the-mqtt-password' | sudo tee /etc/linux-ha-agent/mqtt-password >/dev/null
sudo chmod 600 /etc/linux-ha-agent/mqtt-password
sudo systemctl enable --now linux-ha-agent
journalctl -u linux-ha-agent -f
```

Try it without MQTT: `sudo linux-ha-agent -config /etc/linux-ha-agent/config.yaml -once`
checks everything once and prints the state as JSON.

### NixOS

```nix
{
  inputs.linux-ha-agent.url = "github:lvlie/linux-ha-agent";

  outputs = { nixpkgs, linux-ha-agent, ... }: {
    nixosConfigurations.myhost = nixpkgs.lib.nixosSystem {
      modules = [
        linux-ha-agent.nixosModules.default
        {
          services.linux-ha-agent = {
            enable = true;
            settings = {
              mqtt = {
                url = "ssl://mqtt.lan:8883";
                username = "myhost";
                password_file = "/run/secrets/linux-ha-agent-mqtt"; # sops-nix, agenix, ...
              };
              upgrade.schedule = "30 3 * * 0";
              nixos.flake = "github:me/nix-config"; # or leave empty for channels
            };
          };
        }
      ];
    };
  };
}
```

The configuration is validated when the system is built.

## Configuration

See [`config.example.yaml`](config.example.yaml); only `mqtt.url` is required.
Unknown keys are rejected so typos do not go unnoticed. Validate with
`linux-ha-agent -check-config`.

The most important settings:

| Setting | Default | |
| --- | --- | --- |
| `upgrade.schedule` | off | Cron schedule for *Upgrade host and guests*, e.g. `30 3 * * 0`; prefix `CRON_TZ=Europe/Amsterdam` for a time zone |
| `upgrade.reboot_host` | `false` | Reboot the host when needed after an upgrade |
| `upgrade.restart_guests` | `true` | Restart guests when needed after an upgrade |
| `guests.snapshot_before_upgrade` | `true` | Snapshot guests before upgrading |
| `guests.exclude` | none | Guests to leave alone: `name`, `project/name` (Incus), VMID (Proxmox), wildcards |
| `incus.enabled` / `proxmox.enabled` | `auto` | Manage Incus or Proxmox VE guests when found |
| `permissions.upgrade` / `reboot` / `restore` | `true` / `false` / `false` | What Home Assistant may trigger |
| `checks.interval` | `6h` | How often to look for updates |

## Security

The agent runs as root, so the design keeps what can reach it small:

- **No listening ports.** The agent only makes outgoing connections to the
  broker, the container registries and the local Incus socket.
- **No remote commands.** Home Assistant can only send a fixed set of words
  (`check`, `upgrade`, `upgrade_packages`, `upgrade_containers`, `upgrade_all`,
  `reboot`, `restore_snapshot`); anything else is rejected and logged. Nothing
  received over MQTT ever becomes part of a command line.
- **Permissions** decide which of those Home Assistant may use; reboots and
  restores are off by default. Buttons for disallowed actions are not even
  published.
- **One MQTT user per host, limited by ACL.** With
  [`packaging/mosquitto/acl.conf`](packaging/mosquitto/acl.conf) each agent
  logs in with its host ID and can only publish its own devices and read its
  own commands: a compromised machine cannot command or impersonate another.
  This exact ACL runs in the end-to-end tests.
- **TLS** (`ssl://`), a private CA (`ca_file`) and client certificates are
  supported. Passwords are read from a file, never from the configuration.
- **Guests hold nothing**: no agent, no credentials, no network access needed
  (Proxmox VMs only need the standard QEMU guest agent).
  Every guest upgrade starts with a snapshot you can restore from Home Assistant.
- **Releases** are built in GitHub Actions with checksums and signed build
  provenance (`gh attestation verify`). Actions are pinned to commits;
  Dependabot, govulncheck and zizmor keep dependencies and workflows in check.

Mosquitto setup in short (Home Assistant's Mosquitto add-on works too):

```text
# /etc/mosquitto/conf.d/agents.conf
listener 8883
certfile /etc/mosquitto/certs/server.pem
keyfile  /etc/mosquitto/certs/server.key
allow_anonymous false
password_file /etc/mosquitto/passwd
acl_file /etc/mosquitto/acl.conf
```

```sh
sudo mosquitto_passwd /etc/mosquitto/passwd myhost   # one per agent, named after its host ID
```

The host ID is the host name in lowercase with everything except `a-z`, `0-9`
and `_` replaced by `_` (shown in the logs at startup).

## MQTT topics

| Topic | |
| --- | --- |
| `homeassistant/device/<host>/<device>/config` | Device discovery (retained) |
| `linux-ha-agent/<host>/<device>/state` | Device state as JSON (retained) |
| `linux-ha-agent/<host>/<device>/cmd` | Commands from Home Assistant |
| `linux-ha-agent/<host>/auto_upgrade/set` | `ON` / `OFF` for the schedule |
| `linux-ha-agent/<host>/status` | `online` / `offline` (last will) |

Removed guests are removed from Home Assistant automatically, also when they
were deleted while the agent was down.

## Development

```sh
make test     # unit and integration tests (race detector)
make lint     # pre-commit: golangci-lint, yamllint, shellcheck, actionlint, zizmor, ...
make build    # static binary in dist/
make packages # .deb and .rpm in dist/ (GOARCH=amd64|arm64)
make nix      # nix flake check
```

Tests:

- **Unit tests** for every package, with recorded output of apt, dnf, nix,
  docker and podman.
- **Agent tests** with simulated machines (Ubuntu + docker, Fedora, Alpine,
  stopped and excluded guests) covering checks, upgrades, permissions,
  snapshots, restores, restarts, ordering, lifecycle and persistence, plus a
  simulated Proxmox VE node (LXC through `pct exec`, a VM through the guest
  agent, a VM without guest agent, templates).
- **MQTT integration tests** with an embedded broker: authentication, last
  will, reconnects, and the ACL (another agent cannot command or spoof).
- **Registry tests** against an in-memory OCI registry.
- **Packages**: the `.deb` is installed, reinstalled, removed and purged on
  Ubuntu 24.04 and Debian 13, the `.rpm` on Fedora 43 and Rocky Linux 10
  ([`install-test.sh`](tests/packages/install-test.sh)); `go install` of the
  pushed commit is tested too.
- **End-to-end** ([`e2e.yml`](.github/workflows/e2e.yml)): a GitHub runner
  becomes an Incus host (stable and 6.0 LTS) with Ubuntu + docker, Fedora +
  podman, Debian in another project, Alpine and a stopped guest, and a real
  Mosquitto with the recommended ACL. The test drives the agent over MQTT like
  Home Assistant: discovery, checks, upgrades, snapshot and restore, restarts,
  guest lifecycle and an intruder that must be refused. A NixOS guest is
  tested as well (experimental). Proxmox VE cannot run on GitHub's runners, so
  it is covered by the simulated node, whose `pvesh`, `pct` and `qm` output
  follows the documented formats; it has not yet run against a real node.

When `go.mod` changes, update `vendorHash` in `nix/package.nix` (set it to
`lib.fakeHash`, run `nix build`, copy the hash from the error).

## License

GPL-3.0-or-later
