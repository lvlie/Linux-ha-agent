//go:build e2e

// Package e2e tests a running agent on a real Incus host through a real
// Mosquitto broker, the way Home Assistant uses it. See tests/e2e/setup.sh
// and .github/workflows/e2e.yml for the environment it expects.
package e2e

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lvlie/linux-ha-agent/internal/config"
	"github.com/lvlie/linux-ha-agent/internal/mqtt"
)

var (
	brokerURL = env("E2E_MQTT_URL", "tcp://127.0.0.1:1883")
	hostID    = env("E2E_HOST_ID", "e2e_host")
	base      = "linux-ha-agent/" + hostID
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// broker plays Home Assistant: it records every retained message.
type broker struct {
	mu       sync.Mutex
	retained map[string][]byte
	history  map[string][][]byte
	client   *mqtt.Paho
}

func connect(t *testing.T, user, password string) *mqtt.Paho {
	t.Helper()
	pw := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(pw, []byte(password), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := mqtt.Connect(context.Background(), config.MQTT{URL: brokerURL, Username: user, PasswordFile: pw},
		mqtt.Options{ClientID: "e2e-" + user, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Disconnect)
	return c
}

func homeAssistant(t *testing.T) *broker {
	b := &broker{retained: map[string][]byte{}, history: map[string][][]byte{}}
	b.client = connect(t, env("E2E_HA_USER", "homeassistant"), env("E2E_HA_PASSWORD", "ha"))
	err := b.client.Subscribe("#", func(topic string, payload []byte) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.history[topic] = append(b.history[topic], append([]byte(nil), payload...))
		if len(payload) == 0 {
			delete(b.retained, topic)
			return
		}
		b.retained[topic] = append([]byte(nil), payload...)
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (b *broker) get(topic string) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.retained[topic]
	return p, ok
}

// discovery returns the components of a device's latest discovery message.
func (b *broker) discovery(device string) map[string]map[string]any {
	p, _ := b.get("homeassistant/device/" + hostID + "/" + device + "/config")
	var d struct {
		Cmps map[string]map[string]any `json:"cmps"`
	}
	_ = json.Unmarshal(p, &d)
	return d.Cmps
}

// removedAny reports whether a discovery message of the device deleted a
// component whose key starts with prefix.
func (b *broker) removedAny(device, prefix string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range b.history["homeassistant/device/"+hostID+"/"+device+"/config"] {
		var d struct {
			Cmps map[string]map[string]any `json:"cmps"`
		}
		if json.Unmarshal(p, &d) != nil {
			continue
		}
		for key, c := range d.Cmps {
			if strings.HasPrefix(key, prefix) && len(c) == 1 {
				return true
			}
		}
	}
	return false
}

func (b *broker) state(device string) map[string]any {
	p, ok := b.get(base + "/" + device + "/state")
	if !ok {
		return nil
	}
	var st map[string]any
	if json.Unmarshal(p, &st) != nil {
		return nil
	}
	return st
}

func (b *broker) command(t *testing.T, device, cmd string) {
	t.Helper()
	t.Logf("sending %q to %s", cmd, device)
	if err := b.client.Publish(base+"/"+device+"/cmd", false, []byte(cmd)); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out after %v waiting for %s", timeout, what)
}

func incus(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("incus", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("incus %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// device is the device ID of a guest; "-" in Incus names becomes "_".
func device(name string) string { return hostID + "_" + name }

// waitForRun waits until the device reports a finished run newer than since.
func waitForRun(t *testing.T, b *broker, dev string, since time.Time) map[string]any {
	t.Helper()
	var run map[string]any
	eventually(t, 30*time.Minute, "run on "+dev, func() bool {
		st := b.state(dev)
		if st == nil || st["in_progress"] == true {
			return false
		}
		r, ok := st["last_run"].(map[string]any)
		if !ok {
			return false
		}
		finished, err := time.Parse(time.RFC3339Nano, r["finished"].(string))
		if err != nil || !finished.After(since) {
			return false
		}
		run = r
		return true
	})
	if run["success"] != true {
		pretty, _ := json.MarshalIndent(run, "", "  ")
		t.Fatalf("run on %s failed:\n%s", dev, pretty)
	}
	return run
}

func TestE2E(t *testing.T) {
	b := homeAssistant(t)

	t.Run("discovery", func(t *testing.T) {
		for _, d := range []string{hostID, device("docker_web"), device("podman_fed"), device("lab_deb"), device("alpine"), device("stopped")} {
			eventually(t, 2*time.Minute, "discovery of "+d, func() bool {
				_, ok := b.get("homeassistant/device/" + hostID + "/" + d + "/config")
				return ok
			})
		}
		if p, _ := b.get(base + "/status"); string(p) != "online" {
			t.Fatalf("availability %q", p)
		}
	})

	t.Run("checks", func(t *testing.T) {
		for _, d := range []string{device("docker_web"), device("podman_fed"), device("lab_deb")} {
			eventually(t, 20*time.Minute, "check of "+d, func() bool {
				st := b.state(d)
				return st != nil && st["last_check"] != nil && st["in_progress"] == false
			})
			st := b.state(d)
			if st["check_error"] != "" {
				t.Errorf("%s: check error %v", d, st["check_error"])
			}
			if _, ok := st["updates"].(float64); !ok {
				t.Errorf("%s: no update count: %v", d, st["updates"])
			}
		}
		web := b.state(device("docker_web"))
		if web["package_manager"] != "apt" || web["image_updates"] != 1.0 || web["containers_running"] != 1.0 {
			t.Errorf("docker_web: %v", web)
		}
		if fed := b.state(device("podman_fed")); fed["package_manager"] != "dnf" {
			t.Errorf("podman_fed: %v", fed)
		}
		if st := b.state(device("stopped")); st["running"] != false || st["last_check"] != nil {
			t.Errorf("stopped guest: %v", st)
		}
		if st := b.state(device("alpine")); st["updates"] != nil || !strings.Contains(st["os"].(string), "Alpine") {
			t.Errorf("alpine: %v", st)
		}
	})

	t.Run("system sensors", func(t *testing.T) {
		for _, d := range []string{hostID, device("docker_web"), device("podman_fed"), device("lab_deb"), device("alpine")} {
			// The host is checked after the guests and its apt refresh is slow.
			eventually(t, 10*time.Minute, "system sensors of "+d, func() bool {
				return b.state(d)["kernel"] != nil
			})
			st := b.state(d)
			for _, field := range []string{"kernel", "arch", "boot_time", "cpu_percent", "memory_percent", "disk_percent", "load_1"} {
				if st[field] == nil {
					t.Errorf("%s: %s missing", d, field)
				}
			}
		}
		host := b.state(hostID)
		if host["guests"] != 5.0 || host["guests_running"] != 4.0 || host["updates_total"] == nil {
			t.Errorf("host totals: guests=%v running=%v updates=%v", host["guests"], host["guests_running"], host["updates_total"])
		}
		if _, ok := b.discovery(device("docker_web"))["kernel_update"]; ok {
			t.Error("containers must not get a kernel update sensor")
		}
		if _, ok := b.discovery(hostID)["kernel_update"]; !ok {
			t.Error("host misses the kernel update sensor")
		}
	})

	t.Run("reboot permissions", func(t *testing.T) {
		for d, want := range map[string]bool{hostID: false, device("docker_web"): true, device("alpine"): false} {
			if _, ok := b.discovery(d)["reboot"]; ok != want {
				t.Errorf("%s: reboot button %v, want %v", d, ok, want)
			}
		}
	})

	t.Run("commands run as nobody", func(t *testing.T) {
		whoami := func(d string) map[string]any {
			v, _ := b.state(d)["cmd"].(map[string]any)["cmd_whoami"].(map[string]any)
			return v
		}
		// util-linux setpriv (Ubuntu, Debian) and BusyBox (Alpine) must both work;
		// the Fedora image must at least never run it as root.
		for _, d := range []string{hostID, device("docker_web"), device("lab_deb"), device("alpine")} {
			eventually(t, 5*time.Minute, "whoami on "+d, func() bool { return whoami(d) != nil })
			if v := whoami(d); v["value"] != "nobody" {
				t.Errorf("%s: %v", d, v)
			}
		}
		for _, d := range []string{device("podman_fed")} {
			eventually(t, 5*time.Minute, "whoami on "+d, func() bool { return whoami(d) != nil })
			if v := whoami(d); v["value"] == "root" || (v["value"] != "nobody" && v["error"] == "") {
				t.Errorf("%s: %v", d, v)
			}
		}
		if cmps := b.discovery(device("docker_web")); cmps["run_command"]["p"] != "text" || cmps["cmd_whoami"]["p"] != "sensor" {
			t.Fatalf("command entities missing: %v %v", cmps["run_command"], cmps["cmd_whoami"])
		}
		b.command(t, device("docker_web"), "run_command touch /root/pwned; cat /etc/shadow")
		eventually(t, 2*time.Minute, "typed command result", func() bool {
			c, _ := b.state(device("docker_web"))["command"].(map[string]any)
			return c != nil
		})
		c := b.state(device("docker_web"))["command"].(map[string]any)
		if c["exit_code"] == 0.0 || !strings.Contains(c["output"].(string), "Permission denied") {
			t.Errorf("unprivileged command: %v", c)
		}
		if _, err := exec.Command("incus", "exec", "docker-web", "--", "test", "-e", "/root/pwned").CombinedOutput(); err == nil {
			t.Error("a typed command wrote to /root")
		}
	})

	webCtr := "docker_app_sleeper_1"
	t.Run("container entities", func(t *testing.T) {
		cmps := b.discovery(device("docker_web"))
		upd := cmps["ctr_"+webCtr+"_update"]
		if upd == nil || upd["payload_install"] != "upgrade_container "+webCtr {
			t.Fatalf("container update entity: %v (components: %d)", upd, len(cmps))
		}
		if cmps["ctr_"+webCtr+"_running"] == nil || cmps["ctr_"+webCtr+"_restart"] == nil {
			t.Fatal("container running sensor or restart button missing")
		}
		ctr := b.state(device("docker_web"))["ctr"].(map[string]any)[webCtr].(map[string]any)
		u := ctr["update"].(map[string]any)
		if ctr["running"] != true || u["installed_version"] == u["latest_version"] {
			t.Fatalf("container should run and have a newer image: %v", ctr)
		}
	})

	t.Run("upgrade a single container", func(t *testing.T) {
		oldImage := incus(t, "exec", "docker-web", "--", "docker", "inspect", "--format", "{{.Image}}", "app-sleeper-1")
		since := time.Now()
		b.command(t, device("docker_web"), "upgrade_container "+webCtr)
		waitForRun(t, b, device("docker_web"), since)
		if newImage := incus(t, "exec", "docker-web", "--", "docker", "inspect", "--format", "{{.Image}}", "app-sleeper-1"); newImage == oldImage {
			t.Error("container was not recreated on the newer image")
		}
		eventually(t, 5*time.Minute, "container up to date", func() bool {
			ctr, _ := b.state(device("docker_web"))["ctr"].(map[string]any)[webCtr].(map[string]any)
			u, _ := ctr["update"].(map[string]any)
			return u != nil && u["installed_version"] == u["latest_version"]
		})
	})

	t.Run("restart a container", func(t *testing.T) {
		started := func() string {
			return incus(t, "exec", "docker-web", "--", "docker", "inspect", "--format", "{{.State.StartedAt}}", "app-sleeper-1")
		}
		before := started()
		since := time.Now()
		b.command(t, device("docker_web"), "restart_container "+webCtr)
		waitForRun(t, b, device("docker_web"), since)
		if started() == before {
			t.Error("container was not restarted")
		}
	})

	t.Run("intruder cannot command", func(t *testing.T) {
		intruder := connect(t, env("E2E_INTRUDER_USER", "intruder"), env("E2E_INTRUDER_PASSWORD", "intruder"))
		before := incus(t, "query", "/1.0/instances/deb/state?project=lab")
		_ = intruder.Publish(base+"/"+device("lab_deb")+"/cmd", false, []byte("reboot"))
		time.Sleep(10 * time.Second)
		after := incus(t, "query", "/1.0/instances/deb/state?project=lab")
		if pid(before) != pid(after) {
			t.Fatal("intruder restarted a guest")
		}
	})

	t.Run("upgrade docker guest", func(t *testing.T) {
		since := time.Now()
		b.command(t, device("docker_web"), "upgrade")
		run := waitForRun(t, b, device("docker_web"), since)
		t.Logf("steps: %v", run["steps"])
		if !strings.Contains(incus(t, "snapshot", "list", "docker-web", "--format", "csv"), "ha-agent-") {
			t.Error("no pre-upgrade snapshot")
		}
		eventually(t, 10*time.Minute, "refreshed state", func() bool {
			st := b.state(device("docker_web"))
			return st["image_updates"] == 0.0 && st["updates"] == 0.0
		})
		if out := incus(t, "exec", "docker-web", "--", "docker", "images", "-q", "-f", "dangling=true"); out != "" {
			t.Errorf("dangling images left: %s", out)
		}
	})

	t.Run("upgrade fedora guest with podman", func(t *testing.T) {
		since := time.Now()
		b.command(t, device("podman_fed"), "upgrade")
		waitForRun(t, b, device("podman_fed"), since)
		eventually(t, 10*time.Minute, "fedora up to date", func() bool {
			return b.state(device("podman_fed"))["updates"] == 0.0
		})
	})

	t.Run("upgrade packages in another project", func(t *testing.T) {
		since := time.Now()
		b.command(t, device("lab_deb"), "upgrade_packages")
		waitForRun(t, b, device("lab_deb"), since)
		out := incus(t, "exec", "--project", "lab", "deb", "--", "sh", "-c", "apt-get -s dist-upgrade | grep -c '^Inst' || true")
		if out != "0" {
			t.Errorf("debian still has %s pending updates", out)
		}
	})

	t.Run("restart guest", func(t *testing.T) {
		before := pid(incus(t, "query", "/1.0/instances/deb/state?project=lab"))
		since := time.Now()
		b.command(t, device("lab_deb"), "reboot")
		waitForRun(t, b, device("lab_deb"), since)
		if after := pid(incus(t, "query", "/1.0/instances/deb/state?project=lab")); after == before {
			t.Error("guest was not restarted")
		}
	})

	t.Run("restore snapshot", func(t *testing.T) {
		incus(t, "exec", "docker-web", "--", "touch", "/root/after-upgrade")
		since := time.Now()
		b.command(t, device("docker_web"), "restore_snapshot")
		waitForRun(t, b, device("docker_web"), since)
		if _, err := exec.Command("incus", "exec", "docker-web", "--", "test", "-e", "/root/after-upgrade").CombinedOutput(); err == nil {
			t.Error("file created after the upgrade survived the restore")
		}
	})

	t.Run("removed container entities are deleted", func(t *testing.T) {
		incus(t, "exec", "podman-fed", "--", "sh", "-c", "podman rm -f $(podman ps -aq)")
		b.command(t, device("podman_fed"), "check")
		eventually(t, 5*time.Minute, "container entities of podman-fed deleted", func() bool {
			for key := range b.discovery(device("podman_fed")) {
				if strings.HasPrefix(key, "ctr_") {
					return false
				}
			}
			// podman-compose names containers differently between versions,
			// so look for any container entity that was deleted.
			return b.removedAny(device("podman_fed"), "ctr_")
		})
	})

	t.Run("guest lifecycle", func(t *testing.T) {
		incus(t, "delete", "--force", "alpine")
		eventually(t, 2*time.Minute, "removal of deleted guest", func() bool {
			_, ok := b.get("homeassistant/device/" + hostID + "/" + device("alpine") + "/config")
			return !ok
		})
		incus(t, "start", "stopped")
		eventually(t, 10*time.Minute, "check of started guest", func() bool {
			st := b.state(device("stopped"))
			return st != nil && st["running"] == true && st["last_check"] != nil
		})
	})
}

func pid(stateJSON string) float64 {
	var st struct {
		Pid float64 `json:"pid"`
	}
	_ = json.Unmarshal([]byte(stateJSON), &st)
	return st.Pid
}
