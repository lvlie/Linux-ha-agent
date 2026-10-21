# Home Assistant setup

The agent talks to Home Assistant through an MQTT broker. Home Assistant
creates all devices and entities by itself (MQTT discovery); there is no
custom integration to install.

## 1. Install the Mosquitto broker

In Home Assistant: **Settings → Add-ons → Add-on store → Mosquitto broker →
Install → Start**. Then **Settings → Devices & services**: Home Assistant
finds the broker and offers to set up the **MQTT** integration. Accept it.

Already running your own Mosquitto? Add it with **Settings → Devices &
services → Add integration → MQTT** and skip step 2's add-on specifics.

## 2. Create one MQTT user per machine

Every machine running the agent gets its own login, named after its **host
ID**: the host name in lowercase with everything except `a-z`, `0-9` and `_`
replaced by `_` (`pve-01.lan` → `pve_01_lan`). The agent prints it at startup.

With the add-on: **Settings → People → Users → Add user**, for example
`pve_01_lan`, with a long random password. (Local users are enough; they do
not need to log in to the Home Assistant UI.)

## 3. Restrict what each agent may do (recommended)

Without an access list, every MQTT user can read and write every topic. With
one, each agent can only publish its own devices and read its own commands,
so a compromised machine cannot control or impersonate another one.

With the add-on:

1. In the add-on's **Configuration** tab, enable `customize` with folder
   `mosquitto`:

   ```yaml
   customize:
     active: true
     folder: mosquitto
   ```

2. With the File editor or Samba add-on, create `/share/mosquitto/acl.conf`:

   ```text
   acl_file /share/mosquitto/accesscontrollist
   ```

3. And `/share/mosquitto/accesscontrollist` (the same as
   [`packaging/mosquitto/acl.conf`](../packaging/mosquitto/acl.conf), plus the
   add-on's own users):

   ```text
   user homeassistant
   topic readwrite #

   user addons
   topic readwrite #

   pattern write homeassistant/device/%u/#
   pattern readwrite linux-ha-agent/%u/#
   pattern read homeassistant/status
   ```

4. Restart the add-on.

Your own Mosquitto: add `acl_file /etc/mosquitto/acl.conf` and
`allow_anonymous false` to its configuration and use the same file.

## 4. Point the agent at the broker

On each machine, in `/etc/linux-ha-agent/config.yaml`:

```yaml
mqtt:
  url: tcp://homeassistant.local:1883     # ssl://...:8883 when TLS is set up
  username: pve_01_lan                    # the host ID
  password_file: /etc/linux-ha-agent/mqtt-password
```

```sh
echo 'the-password' | sudo tee /etc/linux-ha-agent/mqtt-password >/dev/null
sudo chmod 600 /etc/linux-ha-agent/mqtt-password
sudo systemctl enable --now linux-ha-agent
```

Within a minute the machine and its guests show up under **Settings →
Devices & services → MQTT**, and pending updates appear under **Settings →
Updates**. See [entities.md](entities.md) for everything that is created.

## 5. Allow actions (optional)

By default Home Assistant may start upgrades, but not reboot machines,
restore snapshots or restart containers. Enable those per machine:

```yaml
permissions:
  upgrade: true
  reboot_host: false          # the machine the agent runs on
  reboot_guests: true         # Incus/Proxmox containers and VMs...
  reboot_exclude:             # ...except these
    - type:vm                 # all VMs (type:lxc: all containers)
    - db-*                    # names, project/name or VMIDs, with wildcards
  restore: true
  restart_containers: true
```

Excluded guests are not restarted after upgrades either
(`upgrade.restart_guests`); their *Reboot required* sensor stays on instead.
Buttons for actions that are not permitted are not created at all.

## 6. Commands (optional)

Like LNXlink's bash module, shell commands can become sensors. They always
run as the user `nobody` without any privileges, so they can only read what
every user may read:

```yaml
commands:
  expose:
    - name: Failed units
      command: systemctl --failed --no-legend | wc -l
      unit: units
      interval: 5m
      machines: [host, guests]   # or guest names/patterns, type:vm, type:lxc
    - name: Nightly backup
      command: find /srv/backup/last -mmin -1500 | grep -q .
      type: binary_sensor        # on when the command exits with 0
  allow_any: true                # a "Run command" box for ad-hoc commands
  allow_any_machines: [host]
```

A sensor shows the first line of the output; the full output, exit code and
time are its attributes.

## Dashboard

Update entities work with the built-in cards. A compact overview, with
`pve_01_lan` replaced by your host ID:

```yaml
type: vertical-stack
cards:
  - type: glance
    title: Homelab updates
    entities:
      - entity: sensor.pve_01_lan_pending_updates_host_and_guests
        name: Updates
      - entity: sensor.pve_01_lan_container_image_updates_host_and_guests
        name: Images
      - entity: sensor.pve_01_lan_guests_with_updates
        name: Guests
      - entity: sensor.pve_01_lan_guests_needing_a_reboot
        name: Reboots
  - type: entities
    entities:
      - update.pve_01_lan_system_updates
      - binary_sensor.pve_01_lan_kernel_update_pending
      - sensor.pve_01_lan_last_upgrade_result
      - button.pve_01_lan_upgrade_host_and_guests
      - switch.pve_01_lan_automatic_upgrades
```

The entity IDs follow the device and entity names; check them under
**Settings → Entities** if you renamed anything.

## Automations

Tell me when something needs updating:

```yaml
alias: Homelab updates available
triggers:
  - trigger: numeric_state
    entity_id: sensor.pve_01_lan_pending_updates_host_and_guests
    above: 0
actions:
  - action: notify.mobile_app_my_phone
    data:
      title: Updates available
      message: >
        {{ states('sensor.pve_01_lan_pending_updates_host_and_guests') }} package updates,
        guests: {{ state_attr('sensor.pve_01_lan_guests_with_updates', 'guests') | join(', ') }}
```

Tell me when an upgrade failed, on any machine:

```yaml
alias: Upgrade failed
triggers:
  - trigger: state
    entity_id:
      - sensor.pve_01_lan_last_upgrade_result
      - sensor.web_last_upgrade_result
    to: failed
actions:
  - action: notify.mobile_app_my_phone
    data:
      title: Upgrade failed
      message: "{{ trigger.to_state.attributes.steps | selectattr('ok', 'false') | map(attribute='detail') | join('; ') }}"
```

Schedule upgrades from Home Assistant instead of the agent (for example only
when nobody is streaming):

```yaml
alias: Weekly homelab upgrade
triggers:
  - trigger: time
    at: "03:30:00"
conditions:
  - condition: time
    weekday: [sun]
  - condition: state
    entity_id: media_player.living_room
    state: "off"
actions:
  - action: button.press
    target:
      entity_id: button.pve_01_lan_upgrade_host_and_guests
```

## Assist, the MCP server and AI assistants

Expose the entities you want under **Settings → Voice assistants → Expose**.
They are then available to Assist and to the Home Assistant MCP server, so an
assistant such as Claude can answer "what needs updating?" or press *Upgrade*
for a guest. The agent only accepts its fixed commands, and only the ones you
permitted, whoever sends them.

## Removing machines, guests and containers

- A guest that is deleted (Incus or Proxmox) disappears from Home Assistant
  together with its entities, also when it was deleted while the agent was
  not running.
- A docker/podman container that is removed loses its entities after the next
  check of its machine.
- Stopped guests stay, with *Running* off.
- To remove a whole machine, stop and uninstall the agent, then delete the
  device in Home Assistant (**Settings → Devices → ⋮ → Delete**).
