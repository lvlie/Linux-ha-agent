# Installation

Every [release](https://github.com/lvlie/linux-ha-agent/releases) has `.deb`
and `.rpm` packages and plain binaries for amd64 and arm64, with SHA-256
checksums and signed build provenance. The binary is static: it runs on any
Linux distribution without dependencies.

## Debian, Ubuntu, Proxmox VE (.deb)

```sh
curl -fsSLO https://github.com/lvlie/linux-ha-agent/releases/download/v0.1.0/linux-ha-agent_0.1.0-1_amd64.deb
sudo apt install ./linux-ha-agent_0.1.0-1_amd64.deb
```

## Fedora, RHEL, Rocky, Alma (.rpm)

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

## Install script

Picks the right package (or the plain binary on other distributions) for
the latest or a given release and verifies it:

```sh
curl -fsSLO https://github.com/lvlie/linux-ha-agent/releases/latest/download/install.sh
less install.sh        # read it first
sudo sh install.sh     # or: sudo sh install.sh v0.1.0
```

## With Go

With Go installed (1.26.7 or newer; older Go versions download it
automatically):

```sh
go install github.com/lvlie/linux-ha-agent/cmd/linux-ha-agent@latest
sudo install -m 0755 "$(go env GOPATH)/bin/linux-ha-agent" /usr/local/bin/
```

Then add the [systemd unit](../packaging/systemd/linux-ha-agent.service) (change
`/usr/bin` to `/usr/local/bin`) and a configuration.

## After installing

```sh
sudoedit /etc/linux-ha-agent/config.yaml
echo 'the-mqtt-password' | sudo tee /etc/linux-ha-agent/mqtt-password >/dev/null
sudo chmod 600 /etc/linux-ha-agent/mqtt-password
sudo systemctl enable --now linux-ha-agent
journalctl -u linux-ha-agent -f
```

Try it without MQTT: `sudo linux-ha-agent -config /etc/linux-ha-agent/config.yaml -once`
checks everything once and prints the state as JSON.

## NixOS

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

See [`config.example.yaml`](../config.example.yaml); only `mqtt.url` is required.
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
| `permissions.upgrade` / `restore` / `restart_containers` | `true` / `false` / `false` | What Home Assistant may trigger |
| `permissions.reboot_host` / `reboot_guests` | `false` / `false` | Reboot buttons for the host and for guests |
| `permissions.reboot_exclude` | none | Guests never restarted (also not after upgrades): names, `project/name`, VMIDs, wildcards, `type:vm`, `type:lxc` |
| `commands.expose` / `commands.allow_any` | none / `false` | Command sensors and ad-hoc commands, run as `nobody` |
| `checks.interval` | `6h` | How often to look for updates |

Next: [connect it to Home Assistant](home-assistant.md).
