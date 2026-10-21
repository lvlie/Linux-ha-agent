// Package ha builds Home Assistant MQTT discovery messages. Every machine is
// one HA device announced with a single device-based discovery message.
package ha

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Command payloads accepted on a device's command topic.
const (
	CmdCheck             = "check"
	CmdUpgrade           = "upgrade"
	CmdUpgradePackages   = "upgrade_packages"
	CmdUpgradeContainers = "upgrade_containers"
	CmdUpgradeAll        = "upgrade_all"
	CmdReboot            = "reboot"
	CmdRestore           = "restore_snapshot"
	// Container commands take the container key as argument: "upgrade_container docker_web".
	CmdUpgradeContainer = "upgrade_container"
	CmdRestartContainer = "restart_container"
	// CmdRunCommand takes a shell command that runs as nobody: "run_command uptime".
	CmdRunCommand = "run_command"
)

// Topics computes the MQTT topics of one agent.
type Topics struct {
	DiscoveryPrefix string
	Base            string
	HostID          string
}

// Config is the discovery config topic of a device. The host ID is used as
// the discovery node ID, so a broker ACL can limit each agent to
// <prefix>/device/<its host id>/#.
func (t Topics) Config(deviceID string) string {
	return t.DiscoveryPrefix + "/device/" + t.HostID + "/" + deviceID + "/config"
}

// State is the retained JSON state of a device.
func (t Topics) State(deviceID string) string {
	return t.Base + "/" + t.HostID + "/" + deviceID + "/state"
}

// Command is where Home Assistant sends commands for a device.
func (t Topics) Command(deviceID string) string {
	return t.Base + "/" + t.HostID + "/" + deviceID + "/cmd"
}

// CommandFilter subscribes to the commands of all devices of this agent.
func (t Topics) CommandFilter() string {
	return t.Base + "/" + t.HostID + "/+/cmd"
}

// Availability is the agent's online/offline topic (also the last will).
func (t Topics) Availability() string {
	return t.Base + "/" + t.HostID + "/status"
}

// AutoUpgrade is the command topic of the automatic upgrade switch.
func (t Topics) AutoUpgrade() string {
	return t.Base + "/" + t.HostID + "/auto_upgrade/set"
}

// HAStatus is Home Assistant's birth topic; on "online" discovery is resent.
func (t Topics) HAStatus() string {
	return t.DiscoveryPrefix + "/status"
}

var nonSlug = regexp.MustCompile(`[^a-z0-9_]+`)

// Slug turns a name into an identifier usable in topics and unique IDs.
func Slug(parts ...string) string {
	s := strings.ToLower(strings.Join(parts, "_"))
	s = nonSlug.ReplaceAllString(s, "_")
	return strings.Trim(s, "_")
}

// Device describes one machine.
type Device struct {
	ID    string
	Name  string
	Model string
	// ViaDevice links guests to their host.
	ViaDevice string
	// Host is true for the machine the agent runs on.
	Host bool
	// Guest is true for Incus and Proxmox guests.
	Guest bool
	// SharedKernel is true for containers (LXC, Incus containers): they run
	// the host's kernel and have no kernel or temperature of their own.
	SharedKernel bool
	// Temperature is true when the machine reports a temperature.
	Temperature bool
	// Engines is true when docker or podman was found.
	Engines bool
	// Containers are the docker/podman containers that get their own entities.
	Containers []Container
	// Commands are the configured command sensors that run on this machine.
	Commands []Command
	// AnyCommand adds the "Run command" text entity and its output sensor.
	AnyCommand bool
}

// Command is a command whose output is a sensor or binary sensor.
type Command struct {
	// Key is the entity key and the key in the state's "cmd" object.
	Key         string
	Name        string
	Binary      bool
	Unit        string
	Icon        string
	DeviceClass string
}

// Container is a docker/podman container with its own entities.
type Container struct {
	// Key identifies the container in commands and the state; see ContainerKey.
	Key  string
	Name string
	// Managed containers can be updated by the agent (compose or podman auto-update).
	Managed bool
}

// ContainerKey is the identifier of a container on one machine.
func ContainerKey(engine, name string) string {
	return Slug(engine, name)
}

// Features enabled for a device.
type Features struct {
	Upgrade           bool
	Reboot            bool
	Restore           bool
	Schedule          bool
	RestartContainers bool
}

// Origin identifies the agent in discovery messages.
type Origin struct {
	Name    string
	Version string
	URL     string
}

type component map[string]any

// Payload builds the device discovery payload. previous holds the components
// (key to platform) of the last published payload: components that are gone
// are sent with only their platform, which makes Home Assistant delete their
// entities. It returns the components that are published now.
func Payload(t Topics, o Origin, d Device, f Features, previous map[string]string) ([]byte, map[string]string, error) {
	state := t.State(d.ID)
	cmd := t.Command(d.ID)
	cmps := map[string]component{}
	add := func(key, platform, name string, c component) {
		c["p"] = platform
		c["name"] = name
		c["unique_id"] = d.ID + "_" + key
		c["availability_topic"] = t.Availability()
		if platform != "button" {
			c["state_topic"] = state
		}
		cmps[key] = c
	}
	button := func(key, name, payload string, extra component) {
		c := component{"command_topic": cmd, "payload_press": payload}
		for k, v := range extra {
			c[k] = v
		}
		add(key, "button", name, c)
	}
	updateEntity := func(key, name, template, install string) {
		c := component{"value_template": template}
		if install != "" {
			c["command_topic"] = cmd
			c["payload_install"] = install
		}
		add(key, "update", name, c)
	}
	measurement := func(key, name, field, unit, icon string, extra component) {
		c := component{"value_template": "{{ value_json." + field + " }}", "state_class": "measurement", "icon": icon}
		if unit != "" {
			c["unit_of_measurement"] = unit
		}
		for k, v := range extra {
			c[k] = v
		}
		add(key, "sensor", name, c)
	}
	installIf := func(ok bool, payload string) string {
		if ok {
			return payload
		}
		return ""
	}

	// Updates.
	updateEntity("system_update", "System updates", "{{ value_json.system_update | tojson }}", installIf(f.Upgrade, CmdUpgradePackages))
	measurement("updates", "Pending updates", "updates", "updates", "mdi:package-up", component{
		"json_attributes_topic":    state,
		"json_attributes_template": "{{ {'packages': value_json.update_list, 'package_manager': value_json.package_manager} | tojson }}",
	})
	measurement("security_updates", "Security updates", "security_updates", "updates", "mdi:shield-alert", nil)
	add("updates_available", "binary_sensor", "Updates available", component{
		"value_template": "{{ 'ON' if value_json.updates_available else 'OFF' }}",
		"device_class":   "update",
	})
	add("reboot_required", "binary_sensor", "Reboot required", component{
		"value_template": "{{ 'ON' if value_json.reboot_required else 'OFF' }}",
		"icon":           "mdi:restart-alert",
	})
	add("upgrade_running", "binary_sensor", "Upgrade running", component{
		"value_template": "{{ 'ON' if value_json.in_progress else 'OFF' }}",
		"device_class":   "running",
	})
	add("last_upgrade", "sensor", "Last upgrade", component{
		"value_template": "{{ value_json.last_run_time }}",
		"device_class":   "timestamp",
	})
	add("last_result", "sensor", "Last upgrade result", component{
		"value_template":           "{{ value_json.last_run_result }}",
		"icon":                     "mdi:clipboard-check-outline",
		"json_attributes_topic":    state,
		"json_attributes_template": "{{ (value_json.last_run or {}) | tojson }}",
	})

	// System.
	add("kernel", "sensor", "Kernel", component{
		"value_template":           "{{ value_json.kernel }}",
		"icon":                     "mdi:penguin",
		"json_attributes_topic":    state,
		"json_attributes_template": "{{ {'latest_installed': value_json.latest_kernel, 'arch': value_json.arch} | tojson }}",
	})
	if !d.SharedKernel {
		add("kernel_update", "binary_sensor", "Kernel update pending", component{
			"value_template": "{{ 'ON' if value_json.kernel_update else 'OFF' }}",
			"device_class":   "update",
		})
	}
	add("boot_time", "sensor", "Last boot", component{
		"value_template": "{{ value_json.boot_time }}",
		"device_class":   "timestamp",
	})
	measurement("cpu", "CPU usage", "cpu_percent", "%", "mdi:cpu-64-bit", component{"suggested_display_precision": 1})
	measurement("memory", "Memory usage", "memory_percent", "%", "mdi:memory", component{
		"suggested_display_precision": 1,
		"json_attributes_topic":       state,
		"json_attributes_template":    "{{ {'total_bytes': value_json.memory_total, 'available_bytes': value_json.memory_available} | tojson }}",
	})
	measurement("disk", "Disk usage", "disk_percent", "%", "mdi:harddisk", component{
		"suggested_display_precision": 1,
		"json_attributes_topic":       state,
		"json_attributes_template":    "{{ {'total_bytes': value_json.disk_total, 'used_bytes': value_json.disk_used} | tojson }}",
	})
	measurement("load_1", "Load (1 min)", "load_1", "", "mdi:gauge", nil)
	measurement("load_5", "Load (5 min)", "load_5", "", "mdi:gauge", nil)
	measurement("load_15", "Load (15 min)", "load_15", "", "mdi:gauge", nil)
	if d.Temperature {
		measurement("temperature", "Temperature", "temperature", "°C", "", component{"device_class": "temperature"})
		delete(cmps["temperature"], "icon")
	}

	// Diagnostics.
	add("last_check", "sensor", "Last check", component{
		"value_template":  "{{ value_json.last_check }}",
		"device_class":    "timestamp",
		"entity_category": "diagnostic",
	})
	add("check_error", "sensor", "Check error", component{
		"value_template":  "{{ value_json.check_error or 'none' }}",
		"entity_category": "diagnostic",
		"icon":            "mdi:alert-circle-outline",
	})
	add("os", "sensor", "Operating system", component{
		"value_template":  "{{ value_json.os }}",
		"entity_category": "diagnostic",
		"icon":            "mdi:linux",
	})
	button("check", "Check for updates", CmdCheck, component{"icon": "mdi:refresh", "entity_category": "diagnostic"})

	// Containers.
	if d.Engines {
		updateEntity("container_update", "Container images", "{{ value_json.container_update | tojson }}", installIf(f.Upgrade, CmdUpgradeContainers))
		measurement("image_updates", "Container image updates", "image_updates", "images", "mdi:docker", component{
			"json_attributes_topic":    state,
			"json_attributes_template": "{{ {'images': value_json.image_update_list, 'unknown': value_json.images_unknown} | tojson }}",
		})
		measurement("containers_running", "Containers running", "containers_running", "", "mdi:package-variant-closed", component{
			"json_attributes_topic":    state,
			"json_attributes_template": "{{ {'total': value_json.containers, 'engines': value_json.engines, 'projects': value_json.projects} | tojson }}",
		})
	}
	for _, c := range d.Containers {
		ref := "(value_json.ctr.get('" + c.Key + "') or {})"
		add("ctr_"+c.Key+"_running", "binary_sensor", "Container "+c.Name, component{
			"value_template":           "{{ 'ON' if " + ref + ".get('running') else 'OFF' }}",
			"device_class":             "running",
			"json_attributes_topic":    state,
			"json_attributes_template": "{{ " + ref + ".get('info', {}) | tojson }}",
		})
		updateEntity("ctr_"+c.Key+"_update", "Container "+c.Name+" image", "{{ ("+ref+".get('update') or {}) | tojson }}",
			installIf(f.Upgrade && c.Managed, CmdUpgradeContainer+" "+c.Key))
		if f.RestartContainers {
			button("ctr_"+c.Key+"_restart", "Restart container "+c.Name, CmdRestartContainer+" "+c.Key, component{"device_class": "restart"})
		}
	}

	// Commands.
	for _, c := range d.Commands {
		ref := "(value_json.cmd.get('" + c.Key + "') or {})"
		cmp := component{
			"json_attributes_topic":    state,
			"json_attributes_template": "{{ " + ref + " | tojson }}",
		}
		if c.Icon != "" {
			cmp["icon"] = c.Icon
		}
		if c.DeviceClass != "" {
			cmp["device_class"] = c.DeviceClass
		}
		if c.Binary {
			cmp["value_template"] = "{{ 'ON' if " + ref + ".get('exit_code') == 0 else 'OFF' }}"
			add(c.Key, "binary_sensor", c.Name, cmp)
			continue
		}
		cmp["value_template"] = "{{ " + ref + ".get('value') }}"
		if c.Unit != "" {
			cmp["unit_of_measurement"] = c.Unit
		}
		add(c.Key, "sensor", c.Name, cmp)
	}
	if d.AnyCommand {
		add("run_command", "text", "Run command", component{
			"command_topic":    cmd,
			"command_template": CmdRunCommand + " {{ value }}",
			"value_template":   "{{ (value_json.command or {}).get('command', '') }}",
			"max":              255,
			"icon":             "mdi:console",
		})
		add("command_output", "sensor", "Command output", component{
			"value_template":           "{{ (value_json.command or {}).get('value') }}",
			"icon":                     "mdi:console-line",
			"json_attributes_topic":    state,
			"json_attributes_template": "{{ (value_json.command or {}) | tojson }}",
		})
	}

	// Actions.
	if f.Upgrade {
		button("upgrade", "Upgrade", CmdUpgrade, component{"icon": "mdi:update"})
	}
	if f.Reboot {
		name := "Reboot"
		if d.Guest {
			name = "Restart"
		}
		button("reboot", name, CmdReboot, component{"device_class": "restart"})
	}

	if d.Guest {
		add("running", "binary_sensor", "Running", component{
			"value_template": "{{ 'ON' if value_json.running else 'OFF' }}",
			"device_class":   "running",
		})
		add("platform", "sensor", "Platform", component{
			"value_template":  "{{ value_json.platform }}",
			"entity_category": "diagnostic",
			"icon":            "mdi:server",
		})
		if f.Restore {
			button("restore_snapshot", "Restore pre-upgrade snapshot", CmdRestore, component{"icon": "mdi:backup-restore"})
		}
	}

	if d.Host {
		add("agent_version", "sensor", "Agent version", component{
			"value_template":  "{{ value_json.agent_version }}",
			"entity_category": "diagnostic",
			"icon":            "mdi:tag",
		})
		measurement("guests", "Managed guests", "guests", "", "mdi:server-network", nil)
		measurement("guests_running", "Guests running", "guests_running", "", "mdi:server-network", nil)
		measurement("guests_with_updates", "Guests with updates", "guests_with_updates", "", "mdi:package-up", component{
			"json_attributes_topic":    state,
			"json_attributes_template": "{{ {'guests': value_json.guests_with_updates_list} | tojson }}",
		})
		measurement("guests_reboot_required", "Guests needing a reboot", "guests_reboot_required", "", "mdi:restart-alert", component{
			"json_attributes_topic":    state,
			"json_attributes_template": "{{ {'guests': value_json.guests_reboot_required_list} | tojson }}",
		})
		measurement("updates_total", "Pending updates (host and guests)", "updates_total", "updates", "mdi:package-up", nil)
		measurement("image_updates_total", "Container image updates (host and guests)", "image_updates_total", "images", "mdi:docker", nil)
		if f.Upgrade {
			button("upgrade_all", "Upgrade host and guests", CmdUpgradeAll, component{"icon": "mdi:update"})
		}
		if f.Schedule {
			add("next_upgrade", "sensor", "Next scheduled upgrade", component{
				"value_template": "{{ value_json.next_upgrade }}",
				"device_class":   "timestamp",
			})
			add("auto_upgrade", "switch", "Automatic upgrades", component{
				"value_template": "{{ 'ON' if value_json.auto_upgrade else 'OFF' }}",
				"command_topic":  t.AutoUpgrade(),
				"payload_on":     "ON",
				"payload_off":    "OFF",
				"state_on":       "ON",
				"state_off":      "OFF",
				"icon":           "mdi:calendar-sync",
			})
		}
	}

	current := make(map[string]string, len(cmps))
	for key, c := range cmps {
		current[key] = c["p"].(string)
	}
	for key, platform := range previous {
		if _, ok := cmps[key]; !ok {
			cmps[key] = component{"p": platform} // removes the entity
		}
	}

	dev := map[string]any{
		"ids":  []string{"linux_ha_agent_" + d.ID},
		"name": d.Name,
		"mf":   o.Name,
		"mdl":  d.Model,
		"sw":   o.Version,
	}
	if d.ViaDevice != "" {
		dev["via_device"] = "linux_ha_agent_" + d.ViaDevice
	}
	payload, err := json.Marshal(map[string]any{
		"dev":  dev,
		"o":    map[string]any{"name": o.Name, "sw": o.Version, "url": o.URL},
		"cmps": cmps,
		"qos":  1,
	})
	return payload, current, err
}
