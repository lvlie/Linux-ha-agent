package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lvlie/linux-ha-agent/internal/config"
	"github.com/lvlie/linux-ha-agent/internal/ha"
	"github.com/lvlie/linux-ha-agent/internal/runner"
)

func TestRebootPermissionsAreSplit(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {
		c.Permissions = config.Permissions{RebootGuests: true, RebootExclude: []string{"dev/*"}}
	})
	e.command("lab1", ha.CmdReboot)
	e.command("lab1_dev_fed", ha.CmdReboot)
	if e.reboots != 0 || len(e.incus.Restarts) != 0 {
		t.Fatalf("host reboots %d, guest restarts %v", e.reboots, e.incus.Restarts)
	}
	e.command("lab1_web", ha.CmdReboot)
	if len(e.incus.Restarts) != 1 {
		t.Fatalf("guest restarts %v", e.incus.Restarts)
	}
	for id, want := range map[string]bool{"lab1": false, "lab1_web": true, "lab1_dev_fed": false} {
		_, ok := components(e.discovery(t, id))["reboot"]
		if ok != want {
			t.Errorf("%s: reboot button %v, want %v", id, ok, want)
		}
	}
}

func TestRebootHostOnly(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Permissions = config.Permissions{RebootHost: true} })
	e.command("lab1_web", ha.CmdReboot)
	e.command("lab1", ha.CmdReboot)
	if e.reboots != 1 || len(e.incus.Restarts) != 0 {
		t.Fatalf("host reboots %d, guest restarts %v", e.reboots, e.incus.Restarts)
	}
	if _, ok := components(e.discovery(t, "lab1_web"))["reboot"]; ok {
		t.Error("guest got a restart button")
	}
}

func TestRebootExcludeStopsAutomaticRestart(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {
		c.Permissions.RebootExclude = []string{"type:lxc"}
	})
	e.command("lab1_web", ha.CmdUpgrade)
	if len(e.incus.Restarts) != 0 {
		t.Fatal("excluded guest restarted after upgrade")
	}
	last := e.state(t, "lab1_web")["last_run"].(map[string]any)
	steps := last["steps"].([]any)
	if detail := steps[len(steps)-1].(map[string]any)["detail"]; !strings.Contains(detail.(string), "excluded from reboots") {
		t.Errorf("last step: %v", steps[len(steps)-1])
	}
}

// unprivileged answers commands run through the nobody wrapper.
func unprivileged(f *runner.Fake, shell string, res runner.Result) {
	f.OnFunc(shell, func(c runner.Cmd) runner.Result {
		if len(c.Args) > 4 && c.Args[4] == shell && strings.Contains(c.Args[2], "setpriv") {
			return res
		}
		return runner.Result{ExitCode: 127}
	})
}

func TestCommandSensors(t *testing.T) {
	e := newEnvWith(t, func(c *config.Config) {
		c.Commands.Expose = []config.Command{
			{Name: "Failed units", Command: "systemctl --failed --no-legend | wc -l", Type: "sensor", Unit: "units", Interval: time.Minute, Machines: []string{"host", "web"}},
			{Name: "Backup ok", Command: "test -f /backup/ok", Type: "binary_sensor", Interval: time.Minute, Machines: []string{"guests"}},
		}
	}, func(e *env) {
		unprivileged(e.host, "systemctl --failed --no-legend | wc -l", runner.Result{Stdout: "2\n"})
		unprivileged(e.incus.FakeRunner("default/web"), "systemctl --failed --no-legend | wc -l", runner.Result{Stdout: "0\n"})
		unprivileged(e.incus.FakeRunner("default/web"), "test -f /backup/ok", runner.Result{ExitCode: 1})
		unprivileged(e.incus.FakeRunner("dev/fed"), "test -f /backup/ok", runner.Result{})
	})
	e.agent.runDueCommands(context.Background())

	cmd := func(id, key string) map[string]any {
		v, _ := e.state(t, id)["cmd"].(map[string]any)[key].(map[string]any)
		return v
	}
	if v := cmd("lab1", "cmd_failed_units"); v["value"] != "2" || v["exit_code"] != 0.0 {
		t.Errorf("host sensor: %v", v)
	}
	if v := cmd("lab1_web", "cmd_failed_units"); v["value"] != "0" {
		t.Errorf("web sensor: %v", v)
	}
	if v := cmd("lab1_web", "cmd_backup_ok"); v["exit_code"] != 1.0 {
		t.Errorf("web binary sensor: %v", v)
	}
	if v := cmd("lab1_dev_fed", "cmd_backup_ok"); v["exit_code"] != 0.0 {
		t.Errorf("fed binary sensor: %v", v)
	}
	if cmd("lab1_dev_fed", "cmd_failed_units") != nil || cmd("lab1", "cmd_backup_ok") != nil {
		t.Error("command ran on a machine it is not configured for")
	}

	host := components(e.discovery(t, "lab1"))
	if c, ok := host["cmd_failed_units"].(map[string]any); !ok || c["p"] != "sensor" || c["unit_of_measurement"] != "units" {
		t.Errorf("host command sensor: %v", host["cmd_failed_units"])
	}
	if _, ok := host["cmd_backup_ok"]; ok {
		t.Error("host got the guests' binary sensor")
	}
	if c, ok := components(e.discovery(t, "lab1_dev_fed"))["cmd_backup_ok"].(map[string]any); !ok || c["p"] != "binary_sensor" {
		t.Errorf("guest binary sensor: %v", c)
	}

	// Commands never run directly, only through the wrapper as nobody.
	for _, c := range e.host.Calls {
		if c.Args[0] != "sh" && strings.Contains(c.String(), "systemctl --failed") {
			t.Fatalf("command ran without dropping privileges: %v", c.Args)
		}
		if strings.Contains(c.String(), "systemctl --failed") && !strings.Contains(c.Args[2], "--no-new-privs") {
			t.Fatalf("wrapper does not set no_new_privs: %q", c.Args[2])
		}
	}

	// Not due again until the interval has passed.
	calls := len(e.host.Calls)
	e.agent.runDueCommands(context.Background())
	if len(e.host.Calls) != calls {
		t.Error("command ran again before its interval")
	}
}

func TestRunAnyCommand(t *testing.T) {
	e := newEnvWith(t, func(c *config.Config) { c.Commands.AllowAny = true }, func(e *env) {
		unprivileged(e.host, "uptime -p", runner.Result{Stdout: "up 3 days\n"})
	})
	if _, ok := components(e.discovery(t, "lab1_web"))["run_command"]; ok {
		t.Error("guest got the run command entity although only the host is allowed")
	}
	text, ok := components(e.discovery(t, "lab1"))["run_command"].(map[string]any)
	if !ok || text["p"] != "text" || text["command_template"] != "run_command {{ value }}" {
		t.Fatalf("text entity: %v", text)
	}
	e.command("lab1_web", "run_command uptime -p")
	e.command("lab1", "run_command uptime -p")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, _ := e.state(t, "lab1")["command"].(map[string]any); c != nil {
			if c["value"] != "up 3 days" || c["command"] != "uptime -p" {
				t.Fatalf("command result: %v", c)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("command result never published")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e.incus.FakeRunner("default/web").Ran("uptime -p") {
		t.Error("command ran on a guest that is not allowed")
	}
}

func TestRunAnyCommandNotAllowed(t *testing.T) {
	e := newEnv(t, nil)
	e.command("lab1", "run_command id")
	time.Sleep(50 * time.Millisecond)
	if e.host.Ran("setpriv") {
		t.Fatal("command ran although allow_any is off")
	}
	if _, ok := components(e.discovery(t, "lab1"))["run_command"]; ok {
		t.Error("run command entity published although not allowed")
	}
}
