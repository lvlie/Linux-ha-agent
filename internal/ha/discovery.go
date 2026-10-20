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
	// Guest is true for Incus guests.
	Guest bool
	// Containers is true when docker or podman was found.
	Containers bool
}

// Features enabled for the whole agent.
type Features struct {
	Upgrade  bool
	Reboot   bool
	Restore  bool
	Schedule bool
}

// Origin identifies the agent in discovery messages.
type Origin struct {
	Name    string
	Version string
	URL     string
}

type component map[string]any

// Payload builds the device discovery payload.
func Payload(t Topics, o Origin, d Device, f Features) ([]byte, error) {
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
	updateEntity := func(key, name, field, install string) {
		c := component{"value_template": "{{ value_json." + field + " | tojson }}"}
		if f.Upgrade {
			c["command_topic"] = cmd
			c["payload_install"] = install
		}
		add(key, "update", name, c)
	}

	updateEntity("system_update", "System updates", "system_update", CmdUpgradePackages)
	add("updates", "sensor", "Pending updates", component{
		"value_template":           "{{ value_json.updates }}",
		"state_class":              "measurement",
		"unit_of_measurement":      "updates",
		"icon":                     "mdi:package-up",
		"json_attributes_topic":    state,
		"json_attributes_template": "{{ {'packages': value_json.update_list, 'package_manager': value_json.package_manager} | tojson }}",
	})
	add("security_updates", "sensor", "Security updates", component{
		"value_template":      "{{ value_json.security_updates }}",
		"state_class":         "measurement",
		"unit_of_measurement": "updates",
		"icon":                "mdi:shield-alert",
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

	if d.Containers {
		updateEntity("container_update", "Container images", "container_update", CmdUpgradeContainers)
		add("image_updates", "sensor", "Container image updates", component{
			"value_template":           "{{ value_json.image_updates }}",
			"state_class":              "measurement",
			"unit_of_measurement":      "images",
			"icon":                     "mdi:docker",
			"json_attributes_topic":    state,
			"json_attributes_template": "{{ {'images': value_json.image_update_list, 'unknown': value_json.images_unknown} | tojson }}",
		})
		add("containers_running", "sensor", "Containers running", component{
			"value_template":           "{{ value_json.containers_running }}",
			"state_class":              "measurement",
			"icon":                     "mdi:package-variant-closed",
			"json_attributes_topic":    state,
			"json_attributes_template": "{{ {'total': value_json.containers, 'engines': value_json.engines, 'projects': value_json.projects} | tojson }}",
		})
	}

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
		add("guests", "sensor", "Managed guests", component{
			"value_template": "{{ value_json.guests }}",
			"state_class":    "measurement",
			"icon":           "mdi:server-network",
		})
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
	payload := map[string]any{
		"dev":  dev,
		"o":    map[string]any{"name": o.Name, "sw": o.Version, "url": o.URL},
		"cmps": cmps,
		"qos":  1,
	}
	return json.Marshal(payload)
}
