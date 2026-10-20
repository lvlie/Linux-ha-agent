package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lvlie/linux-ha-agent/internal/config"
	"github.com/lvlie/linux-ha-agent/internal/guest"
	"github.com/lvlie/linux-ha-agent/internal/ha"
	"github.com/lvlie/linux-ha-agent/internal/runner"
)

const webCtr = "docker_app_web_1"

func TestSystemSensorsAndHostTotals(t *testing.T) {
	e := newEnv(t, nil)
	host := e.state(t, "lab1")
	if host["kernel"] != "6.8.0-45-generic" || host["cpu_percent"] != 50.0 || host["memory_percent"] != 75.0 ||
		host["disk_percent"] != 50.0 || host["load_5"] != 0.2 || host["kernel_update"] != false || host["boot_time"] != "2026-10-20T01:59:00Z" {
		t.Errorf("host system sensors: %v", host)
	}
	// Host 2 + web 2 + fed 1 package updates, one newer image on web.
	if host["updates_total"] != 5.0 || host["image_updates_total"] != 1.0 || host["guests"] != 4.0 || host["guests_running"] != 3.0 {
		t.Errorf("host totals: %v", host)
	}
	if got := host["guests_with_updates_list"]; strings.Join(toStrings(got), ",") != "dev/fed,web" {
		t.Errorf("guests with updates: %v", got)
	}
	web := e.state(t, "lab1_web")
	if web["updates_available"] != true || web["platform"] != "Incus container" {
		t.Errorf("web: %v", web)
	}

	hostCmps := components(e.discovery(t, "lab1"))
	for _, key := range []string{"kernel", "kernel_update", "cpu", "memory", "disk", "load_1", "boot_time", "updates_available", "guests_with_updates", "updates_total"} {
		if _, ok := hostCmps[key]; !ok {
			t.Errorf("host misses %s", key)
		}
	}
	if _, ok := components(e.discovery(t, "lab1_web"))["kernel_update"]; ok {
		t.Error("containers share the host kernel and get no kernel update sensor")
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestKernelUpdateMeansRebootRequired(t *testing.T) {
	e := newEnvWith(t, nil, func(e *env) { stats(e.host, "6.8.0-45-generic", "6.8.0-49-generic") })
	host := e.state(t, "lab1")
	if host["kernel_update"] != true || host["reboot_required"] != true || host["latest_kernel"] != "6.8.0-49-generic" {
		t.Fatalf("host: kernel_update=%v reboot_required=%v", host["kernel_update"], host["reboot_required"])
	}
}

func TestContainerEntities(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Permissions.RestartContainers = true })
	cmps := components(e.discovery(t, "lab1_web"))
	upd, ok := cmps["ctr_"+webCtr+"_update"].(map[string]any)
	if !ok || upd["payload_install"] != ha.CmdUpgradeContainer+" "+webCtr || upd["p"] != "update" {
		t.Fatalf("container update entity: %v", cmps["ctr_"+webCtr+"_update"])
	}
	if btn, ok := cmps["ctr_"+webCtr+"_restart"].(map[string]any); !ok || btn["payload_press"] != ha.CmdRestartContainer+" "+webCtr {
		t.Fatalf("restart button: %v", cmps["ctr_"+webCtr+"_restart"])
	}
	if _, ok := cmps["ctr_"+webCtr+"_running"]; !ok {
		t.Fatal("running sensor missing")
	}
	ctr := e.state(t, "lab1_web")["ctr"].(map[string]any)[webCtr].(map[string]any)
	update := ctr["update"].(map[string]any)
	if ctr["running"] != true || update["installed_version"] != "digest-old" || update["latest_version"] != "digest-new" {
		t.Fatalf("container state: %v", ctr)
	}

	e.command("lab1_web", ha.CmdUpgradeContainer+" "+webCtr)
	r := e.incus.FakeRunner("default/web")
	if !r.Ran("compose -p app -f /root/app/compose.yaml pull --quiet web") || !r.Ran("up -d --no-deps web") {
		t.Fatalf("service not upgraded: %v", r.Calls)
	}
	st := e.state(t, "lab1_web")
	if st["last_run_result"] != "upgraded" || e.web.packageUpgradeRuns != 0 {
		t.Fatalf("single container upgrade: %v", st["last_run"])
	}
	ctr = st["ctr"].(map[string]any)[webCtr].(map[string]any)
	if u := ctr["update"].(map[string]any); u["installed_version"] != u["latest_version"] {
		t.Errorf("container still outdated: %v", u)
	}

	r.On("docker restart c1", runner.Result{})
	e.command("lab1_web", ha.CmdRestartContainer+" "+webCtr)
	if !r.Ran("docker restart c1") {
		t.Error("container not restarted")
	}
}

func TestContainerCommandsAreValidated(t *testing.T) {
	e := newEnv(t, nil) // restart_containers not permitted
	if _, ok := components(e.discovery(t, "lab1_web"))["ctr_"+webCtr+"_restart"]; ok {
		t.Error("restart button published without permission")
	}
	r := e.incus.FakeRunner("default/web")
	before := len(r.Calls)
	for _, payload := range []string{
		ha.CmdRestartContainer + " " + webCtr,  // not permitted
		ha.CmdUpgradeContainer + " nope",       // unknown container
		ha.CmdUpgradeContainer + " ; rm -rf /", // garbage
		ha.CmdUpgrade + " " + webCtr,           // argument on a plain command
		ha.CmdUpgradeContainer,                 // missing argument
	} {
		e.command("lab1_web", payload)
	}
	if len(r.Calls) != before {
		t.Fatalf("a rejected command ran something: %v", r.Calls[before:])
	}
}

func TestRemovedContainersAndGuestsAreCleanedUp(t *testing.T) {
	e := newEnv(t, nil)
	key := "ctr_" + webCtr + "_running"
	if _, ok := components(e.discovery(t, "lab1_web"))[key]; !ok {
		t.Fatal("container entity missing")
	}

	// The container is removed: the next discovery deletes its entities.
	e.web.mu.Lock()
	e.web.containerRemoved = true
	e.web.mu.Unlock()
	e.command("lab1_web", ha.CmdCheck)
	stub, ok := components(e.discovery(t, "lab1_web"))[key].(map[string]any)
	if !ok || len(stub) != 1 || stub["p"] != "binary_sensor" {
		t.Fatalf("expected a removal entry for %s, got %v", key, stub)
	}
	// Once removed it is no longer mentioned.
	e.command("lab1_web", ha.CmdCheck)
	if _, ok := components(e.discovery(t, "lab1_web"))[key]; ok {
		t.Fatal("removed component is still sent")
	}

	// A guest that disappears loses its device.
	e.incus.SetGuests(guest.Guest{Name: "web", Project: "default", Type: "container", Status: "Running"})
	go func() { _ = e.incus.WatchLifecycle(context.Background(), func() { e.agent.enqueue(op{kind: "sync"}) }) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.agent.WaitIdle()
		if _, ok := e.mqtt.Retained(e.agent.Topics().Config("lab1_dev_fed")); !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("removed guest still announced")
}

func TestContainerRemovedWhileAgentWasDown(t *testing.T) {
	e := newEnv(t, nil)
	e.cancel()
	e.web.mu.Lock()
	e.web.containerRemoved = true
	e.web.mu.Unlock()

	// A new agent with the same state file and broker.
	a, err := New(Options{Config: e.cfg, MQTT: e.mqtt, Providers: []guest.Provider{e.incus}, Local: e.host, Hostname: "lab1", Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	a.WaitIdle()
	// Until it is checked, the guest keeps its entities (no flapping on
	// restarts); after the check the container's entities are deleted.
	var removedAt, firstAt = -1, -1
	for i, m := range e.mqtt.Log {
		if m.Topic != a.Topics().Config("lab1_web") || i < restartAt(e) {
			continue
		}
		var d map[string]any
		_ = json.Unmarshal(m.Payload, &d)
		c, _ := components(d)["ctr_"+webCtr+"_update"].(map[string]any)
		if firstAt < 0 {
			firstAt = i
			if len(c) <= 1 {
				t.Fatalf("container entity dropped before the guest was checked: %v", c)
			}
		}
		if len(c) == 1 {
			removedAt = i
		}
	}
	if removedAt < 0 {
		t.Fatal("entity of a container removed while the agent was down was not deleted")
	}
}

// restartAt is the index in the MQTT log where the second agent started.
func restartAt(e *env) int {
	for i := len(e.mqtt.Log) - 1; i >= 0; i-- {
		if e.mqtt.Log[i].Topic == e.agent.Topics().Availability() && string(e.mqtt.Log[i].Payload) == "online" {
			return i
		}
	}
	return 0
}
