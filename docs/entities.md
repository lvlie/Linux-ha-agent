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
| Reboot / Restart | button | Host reboot or guest restart (permission `reboot`) |

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
found by the last check).
