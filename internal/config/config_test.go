package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultsAndOverrides(t *testing.T) {
	cfg, err := Parse([]byte(`
mqtt:
  url: ssl://mqtt.lan:8883
  username: incus1
incus:
  exclude: [build-*, lab/scratch]
  snapshot_expiry: 72h
checks:
  interval: 30m
upgrade:
  schedule: "CRON_TZ=Europe/Amsterdam 30 3 * * 0"
permissions:
  reboot: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HomeAssistant.DiscoveryPrefix != "homeassistant" || cfg.HomeAssistant.BaseTopic != "incus-ha-agent" {
		t.Errorf("discovery defaults not applied: %+v", cfg.HomeAssistant)
	}
	if cfg.Incus.SnapshotKeep != 72*time.Hour || cfg.Checks.Interval != 30*time.Minute {
		t.Errorf("durations not parsed: %v %v", cfg.Incus.SnapshotKeep, cfg.Checks.Interval)
	}
	if !cfg.Permissions.Upgrade || !cfg.Permissions.Reboot || cfg.Permissions.Restore {
		t.Errorf("permissions wrong: %+v", cfg.Permissions)
	}
	if !cfg.Host.Manage || cfg.Incus.Enabled != "auto" || !cfg.Incus.Snapshot {
		t.Errorf("defaults lost: %+v %+v", cfg.Host, cfg.Incus)
	}
}

func TestValidationErrors(t *testing.T) {
	_, err := Parse([]byte(`
mqtt:
  url: http://broker
  password_file: /x
homeassistant:
  base_topic: "bad topic/#"
incus:
  enabled: maybe
  instance_types: [container, pod]
checks:
  interval: 1s
upgrade:
  schedule: "every sunday"
nixos:
  gc_older_than: forever
log_level: loud
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{
		"mqtt.url", "password_file requires", "base_topic", "incus.enabled", `unknown type "pod"`,
		"checks.interval", "upgrade.schedule", "gc_older_than", "log_level",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing error about %q in:\n%v", want, err)
		}
	}
}

func TestUnknownFieldsRejected(t *testing.T) {
	_, err := Parse([]byte("mqtt:\n  url: tcp://b:1883\n  pasword_file: /typo\n"))
	if err == nil || !strings.Contains(err.Error(), "pasword_file") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestEmptyConfigNeedsBroker(t *testing.T) {
	_, err := Parse(nil)
	if err == nil || !strings.Contains(err.Error(), "mqtt.url is required") {
		t.Fatalf("got %v", err)
	}
}

func TestPassword(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(file, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pw, err := MQTT{PasswordFile: file}.Password()
	if err != nil || pw != "s3cret" {
		t.Fatalf("got %q %v", pw, err)
	}
}

func TestGuestSelection(t *testing.T) {
	i := Incus{InstanceTypes: []string{"container"}, Projects: []string{"default", "lab"}, Exclude: []string{"build-*", "lab/scratch"}}
	cases := []struct {
		project, name, typ string
		want               bool
	}{
		{"default", "web", "container", true},
		{"default", "build-42", "container", false},
		{"lab", "scratch", "container", false},
		{"default", "scratch", "container", true},
		{"default", "vm1", "virtual-machine", false},
		{"other", "web", "container", false},
	}
	for _, c := range cases {
		got := i.Includes(c.project, c.typ) && !i.Excluded(c.project, c.name)
		if got != c.want {
			t.Errorf("%s/%s (%s): got %v want %v", c.project, c.name, c.typ, got, c.want)
		}
	}
}
