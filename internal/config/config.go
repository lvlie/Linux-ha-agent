// Package config loads and validates the agent configuration.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"go.yaml.in/yaml/v3"
)

// Config is the agent configuration file.
type Config struct {
	MQTT          MQTT          `yaml:"mqtt"`
	HomeAssistant HomeAssistant `yaml:"homeassistant"`
	Host          Host          `yaml:"host"`
	Guests        Guests        `yaml:"guests"`
	Incus         Incus         `yaml:"incus"`
	Proxmox       Proxmox       `yaml:"proxmox"`
	Checks        Checks        `yaml:"checks"`
	Upgrade       Upgrade       `yaml:"upgrade"`
	NixOS         NixOS         `yaml:"nixos"`
	Permissions   Permissions   `yaml:"permissions"`
	StateFile     string        `yaml:"state_file"`
	LogLevel      string        `yaml:"log_level"`
}

// MQTT connection settings.
type MQTT struct {
	// URL of the broker: tcp://host:1883, ssl://host:8883 (or mqtts://).
	URL                string `yaml:"url"`
	Username           string `yaml:"username"`
	PasswordFile       string `yaml:"password_file"`
	CAFile             string `yaml:"ca_file"`
	ClientCertFile     string `yaml:"client_cert_file"`
	ClientKeyFile      string `yaml:"client_key_file"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	ClientID           string `yaml:"client_id"`
}

// HomeAssistant discovery settings.
type HomeAssistant struct {
	DiscoveryPrefix string `yaml:"discovery_prefix"`
	BaseTopic       string `yaml:"base_topic"`
}

// Host settings.
type Host struct {
	// Name of the host device in Home Assistant; defaults to the hostname.
	Name string `yaml:"name"`
	// Manage the host's own packages and containers.
	Manage bool `yaml:"manage"`
}

// Guests are the settings shared by all virtualisation platforms.
type Guests struct {
	Types        []string      `yaml:"types"`
	Exclude      []string      `yaml:"exclude"`
	Snapshot     bool          `yaml:"snapshot_before_upgrade"`
	SnapshotKeep time.Duration `yaml:"snapshot_expiry"`
	StopTimeout  time.Duration `yaml:"restart_timeout"`
}

// Incus settings.
type Incus struct {
	// Enabled is "auto" (use Incus when the socket exists), "true" or "false".
	Enabled  string   `yaml:"enabled"`
	Socket   string   `yaml:"socket"`
	Projects []string `yaml:"projects"`
}

// Proxmox VE settings.
type Proxmox struct {
	// Enabled is "auto" (use Proxmox VE when /etc/pve exists), "true" or "false".
	Enabled string `yaml:"enabled"`
	// Node name; defaults to the short host name.
	Node string `yaml:"node"`
	// How often to look for guests that were created, removed, started or stopped.
	PollInterval time.Duration `yaml:"poll_interval"`
}

// Checks control how often state is refreshed.
type Checks struct {
	Interval        time.Duration `yaml:"interval"`
	ContainerImages bool          `yaml:"container_images"`
}

// Upgrade controls upgrades.
type Upgrade struct {
	// Schedule is a cron expression (optionally prefixed with CRON_TZ=...)
	// for automatic upgrades of everything. Empty disables the schedule.
	Schedule       string        `yaml:"schedule"`
	AutoUpgrade    bool          `yaml:"auto_upgrade"`
	RebootHost     bool          `yaml:"reboot_host"`
	RestartGuests  bool          `yaml:"restart_guests"`
	Cleanup        bool          `yaml:"cleanup"`
	Containers     bool          `yaml:"containers"`
	CommandTimeout time.Duration `yaml:"command_timeout"`
}

// NixOS settings.
type NixOS struct {
	// Flake to rebuild from, e.g. "github:me/nix-config". Empty uses channels.
	// The hostname is appended as #<hostname> unless the flake has a fragment.
	Flake       string `yaml:"flake"`
	GCOlderThan string `yaml:"gc_older_than"`
}

// Permissions decide which actions Home Assistant may trigger.
type Permissions struct {
	Upgrade bool `yaml:"upgrade"`
	Reboot  bool `yaml:"reboot"`
	Restore bool `yaml:"restore"`
}

// Default returns the configuration with all defaults applied.
func Default() Config {
	return Config{
		HomeAssistant: HomeAssistant{DiscoveryPrefix: "homeassistant", BaseTopic: "linux-ha-agent"},
		Host:          Host{Manage: true},
		Guests: Guests{
			Types:        []string{"container", "virtual-machine"},
			Snapshot:     true,
			SnapshotKeep: 7 * 24 * time.Hour,
			StopTimeout:  2 * time.Minute,
		},
		Incus:   Incus{Enabled: "auto"},
		Proxmox: Proxmox{Enabled: "auto", PollInterval: time.Minute},
		Checks:  Checks{Interval: 6 * time.Hour, ContainerImages: true},
		Upgrade: Upgrade{
			AutoUpgrade:    true,
			RestartGuests:  true,
			Cleanup:        true,
			Containers:     true,
			CommandTimeout: time.Hour,
		},
		NixOS:       NixOS{GCOlderThan: "14d"},
		Permissions: Permissions{Upgrade: true},
		StateFile:   "/var/lib/linux-ha-agent/state.json",
		LogLevel:    "info",
	}
}

// Load reads and validates a configuration file.
func Load(file string) (Config, error) {
	data, err := os.ReadFile(file) //nolint:gosec // the path comes from the command line
	if err != nil {
		return Config{}, fmt.Errorf("reading config: %w", err)
	}
	return Parse(data)
}

// Parse parses and validates configuration data.
func Parse(data []byte) (Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	// An empty file is fine: it leaves the defaults.
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("parsing config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

var (
	topicSafe = regexp.MustCompile(`^[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*$`)
	gcAge     = regexp.MustCompile(`^[0-9]+[dhm]$`)
)

// Validate checks the configuration for mistakes.
func (c *Config) Validate() error {
	var errs []error
	if c.MQTT.URL == "" {
		errs = append(errs, errors.New("mqtt.url is required"))
	} else if u, err := url.Parse(c.MQTT.URL); err != nil || !slices.Contains([]string{"tcp", "mqtt", "ssl", "tls", "mqtts", "ws", "wss"}, u.Scheme) || u.Host == "" {
		errs = append(errs, fmt.Errorf("mqtt.url %q must look like tcp://host:1883 or ssl://host:8883", c.MQTT.URL))
	}
	if c.MQTT.PasswordFile != "" && c.MQTT.Username == "" {
		errs = append(errs, errors.New("mqtt.password_file requires mqtt.username"))
	}
	if (c.MQTT.ClientCertFile == "") != (c.MQTT.ClientKeyFile == "") {
		errs = append(errs, errors.New("mqtt.client_cert_file and mqtt.client_key_file must be set together"))
	}
	if !topicSafe.MatchString(c.HomeAssistant.DiscoveryPrefix) {
		errs = append(errs, fmt.Errorf("homeassistant.discovery_prefix %q is not a valid topic", c.HomeAssistant.DiscoveryPrefix))
	}
	if !topicSafe.MatchString(c.HomeAssistant.BaseTopic) {
		errs = append(errs, fmt.Errorf("homeassistant.base_topic %q is not a valid topic", c.HomeAssistant.BaseTopic))
	}
	if !slices.Contains([]string{"auto", "true", "false"}, c.Incus.Enabled) {
		errs = append(errs, fmt.Errorf("incus.enabled must be auto, true or false, not %q", c.Incus.Enabled))
	}
	if !slices.Contains([]string{"auto", "true", "false"}, c.Proxmox.Enabled) {
		errs = append(errs, fmt.Errorf("proxmox.enabled must be auto, true or false, not %q", c.Proxmox.Enabled))
	}
	if c.Proxmox.PollInterval < 10*time.Second {
		errs = append(errs, errors.New("proxmox.poll_interval must be at least 10s"))
	}
	for _, t := range c.Guests.Types {
		if t != "container" && t != "virtual-machine" {
			errs = append(errs, fmt.Errorf("guests.types: unknown type %q", t))
		}
	}
	for _, pattern := range c.Guests.Exclude {
		if _, err := path.Match(pattern, ""); err != nil {
			errs = append(errs, fmt.Errorf("guests.exclude: bad pattern %q: %w", pattern, err))
		}
	}
	if c.Checks.Interval < time.Minute {
		errs = append(errs, errors.New("checks.interval must be at least 1m"))
	}
	if c.Upgrade.CommandTimeout < time.Minute {
		errs = append(errs, errors.New("upgrade.command_timeout must be at least 1m"))
	}
	if c.Upgrade.Schedule != "" {
		if _, err := ParseSchedule(c.Upgrade.Schedule); err != nil {
			errs = append(errs, fmt.Errorf("upgrade.schedule: %w", err))
		}
	}
	if c.NixOS.GCOlderThan != "" && !gcAge.MatchString(c.NixOS.GCOlderThan) {
		errs = append(errs, fmt.Errorf("nixos.gc_older_than %q must look like 14d", c.NixOS.GCOlderThan))
	}
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, c.LogLevel) {
		errs = append(errs, fmt.Errorf("log_level must be debug, info, warn or error, not %q", c.LogLevel))
	}
	return errors.Join(errs...)
}

// ParseSchedule parses a standard 5-field cron expression, optionally
// prefixed with CRON_TZ=<zone>.
func ParseSchedule(spec string) (cron.Schedule, error) {
	return cron.ParseStandard(spec)
}

// Password reads the MQTT password file, if configured.
func (m MQTT) Password() (string, error) {
	if m.PasswordFile == "" {
		return "", nil
	}
	data, err := os.ReadFile(m.PasswordFile)
	if err != nil {
		return "", fmt.Errorf("reading mqtt password: %w", err)
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

// Excluded reports whether a guest matches one of the exclude patterns.
// Patterns match the name, "project/name" (Incus) or the VMID (Proxmox).
func (g Guests) Excluded(project, name, id string) bool {
	candidates := []string{name}
	if project != "" {
		candidates = append(candidates, project+"/"+name)
	}
	if id != "" {
		candidates = append(candidates, id)
	}
	for _, pattern := range g.Exclude {
		for _, candidate := range candidates {
			if ok, _ := path.Match(pattern, candidate); ok {
				return true
			}
		}
	}
	return false
}

// Includes reports whether guests of this type are managed.
func (g Guests) Includes(guestType string) bool {
	return slices.Contains(g.Types, guestType)
}

// InProject reports whether an Incus project is managed.
func (i Incus) InProject(project string) bool {
	return len(i.Projects) == 0 || slices.Contains(i.Projects, project)
}
