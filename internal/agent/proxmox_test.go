package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lvlie/linux-ha-agent/internal/config"
	"github.com/lvlie/linux-ha-agent/internal/guest"
	"github.com/lvlie/linux-ha-agent/internal/ha"
	"github.com/lvlie/linux-ha-agent/internal/mqtt"
	"github.com/lvlie/linux-ha-agent/internal/proxmox"
	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// unwrapEnv turns "... -- env K=V ... cmd args" back into the inner command.
func unwrapEnv(c runner.Cmd) runner.Cmd {
	args := c.Args
	for i, a := range args {
		if a == "env" {
			args = args[i+1:]
			break
		}
	}
	inner := runner.Cmd{Env: map[string]string{}}
	for len(args) > 0 && strings.Contains(args[0], "=") {
		k, v, _ := strings.Cut(args[0], "=")
		inner.Env[k] = v
		args = args[1:]
	}
	inner.Args = args
	return inner
}

// pveNode simulates a Proxmox VE node: pvesh, pct and qm on the local runner,
// with commands inside guests delegated to simulated machines.
type pveNode struct {
	mu        sync.Mutex
	local     *runner.Fake
	snapshots map[string][]string
	reboots   []string
	rollbacks []string
	execs     map[string]runner.Result
	nextPID   int
}

func newPVENode(ct101, vm201 *runner.Fake) *pveNode {
	n := &pveNode{local: &runner.Fake{}, snapshots: map[string][]string{}, execs: map[string]runner.Result{}, nextPID: 100}
	n.local.
		OnFunc("pct exec 101 -- ", func(c runner.Cmd) runner.Result {
			res, _ := ct101.Run(context.Background(), unwrapEnv(c))
			return res
		}).
		OnFunc("qm guest exec 201 --timeout 0 -- ", func(c runner.Cmd) runner.Result {
			res, _ := vm201.Run(context.Background(), unwrapEnv(c))
			n.mu.Lock()
			defer n.mu.Unlock()
			n.nextPID++
			n.execs[fmt.Sprint(n.nextPID)] = res
			return runner.Result{Stdout: fmt.Sprintf(`{"pid":%d}`, n.nextPID)}
		}).
		OnFunc("qm guest exec-status 201 ", func(c runner.Cmd) runner.Result {
			n.mu.Lock()
			defer n.mu.Unlock()
			res := n.execs[c.Args[len(c.Args)-1]]
			out, _ := json.Marshal(map[string]any{"exited": 1, "exitcode": res.ExitCode, "out-data": res.Stdout, "err-data": res.Stderr})
			return runner.Result{Stdout: string(out)}
		}).
		On("qm guest exec 200", runner.Result{ExitCode: 255, Stderr: "QEMU guest agent is not running\n"}).
		On("pvesh get /nodes/pve1/lxc --output-format json", runner.Result{Stdout: `[
			{"name":"web","status":"running","vmid":"101"},
			{"name":"media","status":"stopped","vmid":"104"},
			{"name":"sonarr","status":"running","vmid":"105"},
			{"name":"tmpl","status":"stopped","template":1,"vmid":"900"}]`}).
		On("cat /etc/pve/nodes/pve1/lxc/105.conf", runner.Result{Stdout: "#image%3A ghcr.io/linuxserver/sonarr%3Alatest\nentrypoint: /init\nostype: unmanaged\n"}).
		On("cat /etc/pve/nodes/pve1/lxc/", runner.Result{Stdout: "ostype: ubuntu\n"}).
		On("pvesh get /nodes/pve1/qemu --output-format json", runner.Result{Stdout: `[
			{"name":"win11","status":"running","vmid":200},
			{"name":"docker-vm","status":"running","vmid":201}]`}).
		On("pvesh get /nodes/pve1/qemu/200/config", runner.Result{Stdout: `{"agent":"0"}`}).
		On("pvesh get /nodes/pve1/qemu/201/config", runner.Result{Stdout: `{"agent":"1"}`}).
		OnFunc(" snapshot ", func(c runner.Cmd) runner.Result {
			n.mu.Lock()
			defer n.mu.Unlock()
			n.snapshots[c.Args[2]] = append(n.snapshots[c.Args[2]], c.Args[3])
			return runner.Result{}
		}).
		OnFunc("/snapshot --output-format json", func(c runner.Cmd) runner.Result {
			n.mu.Lock()
			defer n.mu.Unlock()
			id := strings.Split(c.Args[2], "/")[4]
			var entries []map[string]any
			for i, name := range n.snapshots[id] {
				entries = append(entries, map[string]any{"name": name, "snaptime": 1760000000 + i, "description": proxmox.SnapshotDescription + "2099-01-01T00:00:00Z"})
			}
			entries = append(entries, map[string]any{"name": "current", "description": "You are here!"})
			out, _ := json.Marshal(entries)
			return runner.Result{Stdout: string(out)}
		}).
		OnFunc(" reboot ", func(c runner.Cmd) runner.Result {
			n.mu.Lock()
			defer n.mu.Unlock()
			n.reboots = append(n.reboots, c.Args[0]+" "+c.Args[2])
			return runner.Result{}
		}).
		OnFunc(" rollback ", func(c runner.Cmd) runner.Result {
			n.mu.Lock()
			defer n.mu.Unlock()
			n.rollbacks = append(n.rollbacks, c.String())
			return runner.Result{}
		})
	return n
}

func TestProxmoxNode(t *testing.T) {
	web, vm := &machine{}, &machine{}
	ct101, vm201 := &runner.Fake{}, &runner.Fake{}
	n := newPVENode(ct101, vm201)
	web.ubuntu(ct101)
	web.docker(ct101)
	web.rebootedAfter = func() bool { n.mu.Lock(); defer n.mu.Unlock(); return len(n.reboots) > 0 }
	vm.fedora(vm201)
	(&machine{}).ubuntu(n.local) // the Proxmox host itself (Debian, apt)

	provider, err := proxmox.New(context.Background(), n.local, "pve1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.MQTT.URL = "tcp://broker:1883"
	cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	cfg.Permissions = config.Permissions{Upgrade: true, Reboot: true, Restore: true}
	mem := mqtt.NewMemory()
	reg := registry{"nginx:1.27": "sha256:digest-new", "ghcr.io/linuxserver/sonarr:latest": "sha256:sonarr-1"}
	a, err := New(Options{
		Config: cfg, MQTT: mem, Providers: []guest.Provider{provider}, Local: n.local,
		Registry: reg, Hostname: "pve1", Logger: quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	a.WaitIdle()
	e := &env{agent: a, mqtt: mem}

	configs := mem.RetainedTopics("homeassistant/device/pve1/+/config")
	if len(configs) != 6 { // host, web, media, sonarr, win11, docker-vm; no template
		t.Fatalf("discovery: %v", configs)
	}
	if mdl := e.discovery(t, "pve1_web")["dev"].(map[string]any)["mdl"]; mdl != "Ubuntu 24.04.3 LTS (Proxmox LXC)" {
		t.Errorf("model %v", mdl)
	}
	if st := e.state(t, "pve1_web"); st["updates"] != 2.0 || st["image_updates"] != 1.0 {
		t.Errorf("web state %v", st)
	}
	if st := e.state(t, "pve1_docker_vm"); st["package_manager"] != "dnf" || st["updates"] != 1.0 {
		t.Errorf("vm state %v", st)
	}
	if st := e.state(t, "pve1_win11"); !strings.Contains(st["check_error"].(string), "guest agent is not enabled") {
		t.Errorf("win11 state %v", st)
	}

	// The OCI application container: checked against the registry, nothing
	// runs inside it, and the image it was created from is assumed to run.
	sonarr := e.state(t, "pve1_sonarr")
	u, app := sonarr["app_update"].(map[string]any), sonarr["app"].(map[string]any)
	if sonarr["platform"] != "Proxmox OCI container" || sonarr["check_error"] != "" || sonarr["updates_available"] != false ||
		u["installed_version"] != "ghcr.io/linuxserver/sonarr:latest (sonarr-1)" || u["latest_version"] != u["installed_version"] ||
		app["digest"] != "sha256:sonarr-1" || app["digest_assumed"] != true {
		t.Fatalf("sonarr state: %v", sonarr)
	}
	cmps := components(e.discovery(t, "pve1_sonarr"))
	for _, key := range []string{"upgrade", "restore_snapshot", "system_update"} {
		if _, ok := cmps[key]; ok {
			t.Errorf("sonarr cannot be updated by the agent, but got %s", key)
		}
	}
	if c := cmps["app_update"].(map[string]any); c["payload_install"] != nil {
		t.Errorf("sonarr image entity has an install action: %v", c)
	}
	if n.local.Ran("pct exec 105") {
		t.Error("commands ran inside the application container")
	}
	reg["ghcr.io/linuxserver/sonarr:latest"] = "sha256:sonarr-2"
	e.command("pve1_sonarr", ha.CmdCheck)
	sonarr = e.state(t, "pve1_sonarr")
	u = sonarr["app_update"].(map[string]any)
	if sonarr["updates_available"] != true || u["installed_version"] != "ghcr.io/linuxserver/sonarr:latest (sonarr-1)" || u["latest_version"] != "ghcr.io/linuxserver/sonarr:latest (sonarr-2)" {
		t.Fatalf("sonarr with a newer image: %v", sonarr)
	}

	e.command("pve1_web", ha.CmdUpgrade)
	if st := e.state(t, "pve1_web"); st["last_run_result"] != "upgraded" || st["updates"] != 0.0 || st["image_updates"] != 0.0 {
		t.Fatalf("web after upgrade: %v", st["last_run"])
	}
	if len(n.snapshots["101"]) != 1 || !strings.HasPrefix(n.snapshots["101"][0], SnapshotPrefix) {
		t.Errorf("snapshots %v", n.snapshots)
	}
	if len(n.reboots) != 1 || n.reboots[0] != "pct 101" {
		t.Errorf("reboots %v", n.reboots)
	}

	e.command("pve1_docker_vm", ha.CmdUpgrade)
	if st := e.state(t, "pve1_docker_vm"); st["last_run_result"] != "upgraded" || !vm.upgraded {
		t.Fatalf("vm after upgrade: %v", st["last_run"])
	}

	e.command("pve1_win11", ha.CmdUpgrade)
	if st := e.state(t, "pve1_win11"); st["last_run_result"] != "failed" {
		t.Errorf("win11 upgrade must fail: %v", st["last_run"])
	}
	if len(n.snapshots["200"]) != 0 {
		t.Error("no snapshot for a guest that cannot be upgraded")
	}

	e.command("pve1_web", ha.CmdRestore)
	if len(n.rollbacks) != 1 || !strings.HasPrefix(n.rollbacks[0], "pct rollback 101 "+SnapshotPrefix) || !strings.HasSuffix(n.rollbacks[0], "--start 1") {
		t.Errorf("rollbacks %v", n.rollbacks)
	}

	e.command("pve1", ha.CmdUpgradeAll)
	if st := e.state(t, "pve1_sonarr"); st["last_run"] != nil {
		t.Errorf("upgrade of host and guests must leave sonarr alone: %v", st["last_run"])
	}
}
