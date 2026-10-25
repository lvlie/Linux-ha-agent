# Entities

Every machine (the host running the agent, and each Incus or Proxmox guest)
is a device in Home Assistant. Guests are linked to their host.

## Every machine

| Entity | Type | Notes |
| --- | --- | --- |
| System updates | update | Pending packages; *Install* upgrades them (permission `upgrade`) |
| Pending updates | sensor | Count, package list as attribute |
| Security updates | sensor | apt and dnf; unknown on NixOS |
| Updates available | binary sensor | Packages or container images |
| Reboot required | binary sensor | `/var/run/reboot-required`, `dnf needs-restarting`, NixOS booted system, or a newer kernel installed |
| Kernel | sensor | Running kernel; newest installed kernel and architecture as attributes |
| Kernel update pending | binary sensor | Newer kernel installed than running; not for containers, which share the host kernel |
| Last boot | sensor (timestamp) | |
| CPU usage, Memory usage, Disk usage | sensor (%) | Disk is the root file system; totals as attributes |
| Load (1, 5, 15 min) | sensor | |
| Temperature | sensor (°C) | Hottest thermal zone, when the machine has one (not for containers) |
| Upgrade running | binary sensor | |
| Last upgrade, Last upgrade result | sensor | Result is `upgraded`, `up to date`, `failed` or `never`; every step as attributes |
| Last check, Check error, Operating system | diagnostic sensors | |
| Check for updates | button | |
| Upgrade | button | Packages and containers (permission `upgrade`) |
| Reboot / Restart | button | Host reboot (permission `reboot_host`) or guest restart (`reboot_guests`, unless in `reboot_exclude`) |
| *Command name* | sensor or binary sensor | Each `commands.expose` entry for this machine; first output line, or on when it exits with 0. Output, exit code and time as attributes |
| Run command | text | With `commands.allow_any`; runs what is typed as `nobody` |
| Command output | sensor | First line of the last typed command; output, exit code and time as attributes |

## Machines with Docker or Podman

| Entity | Type | Notes |
| --- | --- | --- |
| Container images | update | Images with a newer version in their registry; *Install* updates all containers |
| Container image updates | sensor | Count, images as attributes |
| Containers running | sensor | Total, engines and compose projects as attributes |
| Container *name* | binary sensor | Running; image, engine, compose project and service as attributes |
| Container *name* image | update | Installed and newest digest; *Install* updates just this container (compose service or podman auto-update) |
| Restart container *name* | button | Permission `restart_containers` |

Container entities can be turned off with `checks.container_entities: false`.

## Guests

| Entity | Type | Notes |
| --- | --- | --- |
| Running | binary sensor | |
| Platform | diagnostic sensor | Incus container/VM, Proxmox LXC/VM |
| Restore pre-upgrade snapshot | button | Permission `restore` |

## Incus OCI application containers

Containers launched from OCI images (`incus launch docker:...`, incus-compose)
are guests with these entities instead of the package, system and command
ones above:

| Entity | Type | Notes |
| --- | --- | --- |
| Image | update | Running and newest image fingerprint; image, compose project and service as attributes. *Install* rebuilds the container from the newer image, or runs incus-compose for its service |
| Updates available | binary sensor | |
| Upgrade running, Last upgrade, Last upgrade result | | As for machines |
| Running, Platform | | Platform is *Incus OCI container* or *incus-compose service* |
| Upgrade, Restart | button | No restore: upgrades rebuild (Incus refuses that with snapshots) or recreate (incus-compose) the container |

## The host

| Entity | Type | Notes |
| --- | --- | --- |
| Upgrade host and guests | button | Host first, then every running guest |
| Automatic upgrades | switch | When `upgrade.schedule` is set |
| Next scheduled upgrade | sensor (timestamp) | |
| Managed guests, Guests running | sensor | |
| Guests with updates, Guests needing a reboot | sensor | Guest names as attributes |
| Pending updates (host and guests), Container image updates (host and guests) | sensor | Totals |
| Agent version | diagnostic sensor | |

## Commands

Buttons and update entities send these payloads to
`linux-ha-agent/<host>/<device>/cmd`; anything else is rejected and logged:

`check`, `upgrade`, `upgrade_packages`, `upgrade_containers`, `upgrade_all`
(host only), `reboot`, `restore_snapshot` (guests only),
`upgrade_container <key>`, `restart_container <key>` (key of a container
found by the last check), `run_command <shell command>` (only with
`commands.allow_any`; runs as `nobody`).
