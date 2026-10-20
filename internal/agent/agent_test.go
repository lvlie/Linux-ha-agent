package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lvlie/linux-ha-agent/internal/config"
	"github.com/lvlie/linux-ha-agent/internal/guest"
	"github.com/lvlie/linux-ha-agent/internal/ha"
	"github.com/lvlie/linux-ha-agent/internal/mqtt"
	"github.com/lvlie/linux-ha-agent/internal/runner"
)

const (
	ubuntuRelease = "NAME=\"Ubuntu\"\nID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"24.04\"\nPRETTY_NAME=\"Ubuntu 24.04.3 LTS\"\n"
	fedoraRelease = "NAME=\"Fedora Linux\"\nID=fedora\nVERSION_ID=43\nPRETTY_NAME=\"Fedora Linux 43 (Container Image)\"\n"
	alpineRelease = "NAME=\"Alpine Linux\"\nID=alpine\nVERSION_ID=3.22.0\nPRETTY_NAME=\"Alpine Linux v3.22\"\n"
)

// machine simulates a Linux machine that changes when it is upgraded.
type machine struct {
	mu                 sync.Mutex
	upgraded           bool
	imagesUpgraded     bool
	pruned             bool
	rebootedAfter      func() bool
	packageUpgradeRuns int
}

func (m *machine) lock() func() { m.mu.Lock(); return m.mu.Unlock }

func (m *machine) ubuntu(f *runner.Fake) {
	f.On("cat /etc/os-release", runner.Result{Stdout: ubuntuRelease}).
		On("Timeout=600 update", runner.Result{}).
		OnFunc("-s dist-upgrade", func(runner.Cmd) runner.Result {
			defer m.lock()()
			if m.upgraded {
				return runner.Result{Stdout: "0 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n"}
			}
			return runner.Result{Stdout: "Inst openssl [3.0.13-1] (3.0.13-2 Ubuntu:24.04/noble-security [amd64])\n" +
				"Inst tzdata [2025a] (2025b Ubuntu:24.04/noble-updates [all])\n"}
		}).
		OnFunc("/var/run/reboot-required", func(runner.Cmd) runner.Result {
			defer m.lock()()
			if m.upgraded && (m.rebootedAfter == nil || !m.rebootedAfter()) {
				return runner.Result{ExitCode: 0}
			}
			return runner.Result{ExitCode: 1}
		}).
		OnFunc("--force-confdef", func(runner.Cmd) runner.Result {
			defer m.lock()()
			m.packageUpgradeRuns++
			if m.upgraded {
				return runner.Result{Stdout: "0 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n"}
			}
			m.upgraded = true
			return runner.Result{Stdout: "2 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n"}
		}).
		On("autoremove --purge", runner.Result{}).
		On("Timeout=600 clean", runner.Result{})
}

func (m *machine) docker(f *runner.Fake) {
	f.On(`sh docker`, runner.Result{}).
		On("docker version --format", runner.Result{Stdout: "27.5.1\n"}).
		On("docker --version", runner.Result{Stdout: "Docker version 27.5.1\n"}).
		On("docker ps -a -q --no-trunc", runner.Result{Stdout: "c1\n"}).
		OnFunc("docker container inspect c1", func(runner.Cmd) runner.Result {
			defer m.lock()()
			id := "sha256:old"
			if m.imagesUpgraded {
				id = "sha256:new"
			}
			return runner.Result{Stdout: fmt.Sprintf(`[{"Id":"c1","Name":"/app-web-1","Image":%q,
				"Config":{"Image":"nginx:1.27","Labels":{"com.docker.compose.project":"app",
				"com.docker.compose.project.working_dir":"/root/app",
				"com.docker.compose.project.config_files":"/root/app/compose.yaml"}},
				"State":{"Status":"running"}}]`, id)}
		}).
		OnFunc("docker image inspect", func(c runner.Cmd) runner.Result {
			defer m.lock()()
			id, digest := "sha256:old", "sha256:digest-old"
			if m.imagesUpgraded {
				id, digest = "sha256:new", "sha256:digest-new"
			}
			return runner.Result{Stdout: fmt.Sprintf(`[{"Id":%q,"RepoDigests":["nginx@%s"]}]`, id, digest)}
		}).
		On("pull --quiet", runner.Result{}).
		OnFunc("up -d --remove-orphans", func(runner.Cmd) runner.Result {
			defer m.lock()()
			m.imagesUpgraded = true
			return runner.Result{}
		}).
		OnFunc("docker image prune", func(runner.Cmd) runner.Result {
			defer m.lock()()
			if m.pruned {
				return runner.Result{Stdout: "Total reclaimed space: 0B\n"}
			}
			m.pruned = true
			return runner.Result{Stdout: "Deleted: sha256:old\nTotal reclaimed space: 50MB\n"}
		})
}

func (m *machine) fedora(f *runner.Fake) {
	f.On("cat /etc/os-release", runner.Result{Stdout: fedoraRelease}).
		On("makecache", runner.Result{}).
		OnFunc("repoquery", func(runner.Cmd) runner.Result {
			defer m.lock()()
			if m.upgraded {
				return runner.Result{}
			}
			return runner.Result{Stdout: "glibc|2.42-5.fc43|updates\n"}
		}).
		On("updateinfo", runner.Result{Stdout: "FEDORA-2025-1 Important/Sec. glibc-2.42-5.fc43.x86_64\n"}).
		On("needs-restarting", runner.Result{ExitCode: 0}).
		OnFunc("upgrade --refresh", func(runner.Cmd) runner.Result {
			defer m.lock()()
			m.upgraded = true
			return runner.Result{Stdout: "Upgraded glibc\nComplete!\n"}
		}).
		On("autoremove", runner.Result{}).
		On("clean packages", runner.Result{})
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type registry map[string]string

func (r registry) Digest(_ context.Context, ref string) (string, error) {
	if d, ok := r[ref]; ok {
		return d, nil
	}
	return "", errors.New("not found")
}

type env struct {
	agent   *Agent
	mqtt    *mqtt.Memory
	incus   *guest.Fake
	host    *runner.Fake
	hostM   *machine
	web     *machine
	fed     *machine
	reboots int
	cancel  context.CancelFunc
	cfg     config.Config
}

func newEnv(t *testing.T, mutate func(*config.Config)) *env {
	t.Helper()
	cfg := config.Default()
	cfg.MQTT.URL = "tcp://broker:1883"
	cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	cfg.Guests.Exclude = []string{"build-*"}
	cfg.Permissions = config.Permissions{Upgrade: true, Reboot: true, Restore: true}
	if mutate != nil {
		mutate(&cfg)
	}
	e := &env{mqtt: mqtt.NewMemory(), host: &runner.Fake{}, hostM: &machine{}, web: &machine{}, fed: &machine{}, cfg: cfg}
	e.incus = guest.NewFake(
		guest.Guest{Name: "web", Project: "default", Type: "container", Status: "Running"},
		guest.Guest{Name: "fed", Project: "dev", Type: "container", Status: "Running"},
		guest.Guest{Name: "alp", Project: "default", Type: "container", Status: "Running"},
		guest.Guest{Name: "old", Project: "default", Type: "container", Status: "Stopped"},
		guest.Guest{Name: "build-1", Project: "default", Type: "container", Status: "Running"},
	)
	e.hostM.ubuntu(e.host)
	e.hostM.rebootedAfter = func() bool { return e.reboots > 0 }
	e.web.ubuntu(e.incus.FakeRunner("default/web"))
	e.web.docker(e.incus.FakeRunner("default/web"))
	e.web.rebootedAfter = func() bool { return len(e.incus.Restarts) > 0 }
	e.fed.fedora(e.incus.FakeRunner("dev/fed"))
	e.incus.FakeRunner("default/alp").On("cat /etc/os-release", runner.Result{Stdout: alpineRelease})

	a, err := New(Options{
		Config:     cfg,
		MQTT:       e.mqtt,
		Providers:  []guest.Provider{e.incus},
		Local:      e.host,
		Registry:   registry{"nginx:1.27": "sha256:digest-new"},
		Hostname:   "Lab1",
		Version:    "1.2.3",
		Now:        func() time.Time { return time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC) },
		RebootHost: func(context.Context) error { e.reboots++; return nil },
		Logger:     quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.agent = a
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	t.Cleanup(cancel)
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	a.WaitIdle()
	return e
}

func (e *env) state(t *testing.T, id string) map[string]any {
	t.Helper()
	payload, ok := e.mqtt.Retained(e.agent.Topics().State(id))
	if !ok {
		t.Fatalf("no state for %s", id)
	}
	var st map[string]any
	if err := json.Unmarshal(payload, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func (e *env) discovery(t *testing.T, id string) map[string]any {
	t.Helper()
	payload, ok := e.mqtt.Retained(e.agent.Topics().Config(id))
	if !ok {
		t.Fatalf("no discovery for %s", id)
	}
	var d map[string]any
	if err := json.Unmarshal(payload, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func (e *env) command(id, cmd string) {
	_ = e.mqtt.Publish(e.agent.Topics().Command(id), false, []byte(cmd))
	e.agent.WaitIdle()
}

func components(d map[string]any) map[string]any {
	return d["cmps"].(map[string]any)
}

func TestDiscoveryOfHostAndGuests(t *testing.T) {
	e := newEnv(t, nil)
	configs := e.mqtt.RetainedTopics("homeassistant/device/+/+/config")
	want := map[string]bool{
		"homeassistant/device/lab1/lab1/config":         true,
		"homeassistant/device/lab1/lab1_web/config":     true,
		"homeassistant/device/lab1/lab1_dev_fed/config": true,
		"homeassistant/device/lab1/lab1_alp/config":     true,
		"homeassistant/device/lab1/lab1_old/config":     true,
	}
	if len(configs) != len(want) {
		t.Fatalf("unexpected discovery topics %v", configs)
	}
	for _, c := range configs {
		if !want[c] {
			t.Errorf("unexpected discovery topic %s", c)
		}
	}

	host := e.discovery(t, "lab1")
	if host["dev"].(map[string]any)["name"] != "Lab1" {
		t.Errorf("host name: %v", host["dev"])
	}
	for _, key := range []string{"upgrade_all", "auto_upgrade", "agent_version", "system_update"} {
		if key == "auto_upgrade" {
			if _, ok := components(host)[key]; ok {
				t.Errorf("auto_upgrade switch without schedule")
			}
			continue
		}
		if _, ok := components(host)[key]; !ok {
			t.Errorf("host misses %s", key)
		}
	}

	web := e.discovery(t, "lab1_web")
	dev := web["dev"].(map[string]any)
	if dev["via_device"] != "linux_ha_agent_lab1" || dev["mdl"] != "Ubuntu 24.04.3 LTS (Incus container)" {
		t.Errorf("guest device: %v", dev)
	}
	for _, key := range []string{"container_update", "image_updates", "running", "restore_snapshot", "reboot"} {
		if _, ok := components(web)[key]; !ok {
			t.Errorf("web misses %s", key)
		}
	}
	if _, ok := components(e.discovery(t, "lab1_alp"))["container_update"]; ok {
		t.Error("alpine has no container engine and must not get container entities")
	}
	update := components(web)["system_update"].(map[string]any)
	if update["command_topic"] != "linux-ha-agent/lab1/lab1_web/cmd" || update["payload_install"] != ha.CmdUpgradePackages ||
		update["availability_topic"] != "linux-ha-agent/lab1/status" {
		t.Errorf("update entity: %v", update)
	}
	if v, _ := e.mqtt.Retained("linux-ha-agent/lab1/status"); string(v) != "online" {
		t.Errorf("availability %q", v)
	}
}

func TestCheckState(t *testing.T) {
	e := newEnv(t, nil)
	web := e.state(t, "lab1_web")
	if web["updates"] != 2.0 || web["security_updates"] != 1.0 || web["package_manager"] != "apt" ||
		web["image_updates"] != 1.0 || web["containers_running"] != 1.0 || web["reboot_required"] != false {
		t.Fatalf("web state: %v", web)
	}
	sys := web["system_update"].(map[string]any)
	if sys["latest_version"] != "Ubuntu 24.04.3 LTS + 2 updates" || sys["installed_version"] != "Ubuntu 24.04.3 LTS" {
		t.Errorf("system update: %v", sys)
	}
	fed := e.state(t, "lab1_dev_fed")
	if fed["updates"] != 1.0 || fed["security_updates"] != 1.0 || fed["package_manager"] != "dnf" {
		t.Errorf("fedora state: %v", fed)
	}
	alp := e.state(t, "lab1_alp")
	if alp["updates"] != nil || alp["os"] != "Alpine Linux v3.22" || alp["check_error"] != "" {
		t.Errorf("alpine state: %v", alp)
	}
	old := e.state(t, "lab1_old")
	if old["running"] != false || old["last_check"] != nil {
		t.Errorf("stopped guest must not be checked: %v", old)
	}
	if e.incus.FakeRunner("default/old").Ran("os-release") {
		t.Error("commands ran in a stopped guest")
	}
	if e.incus.FakeRunner("default/build-1").Ran("os-release") {
		t.Error("excluded guest was checked")
	}
	host := e.state(t, "lab1")
	if host["agent_version"] != "1.2.3" || host["guests"] != 4.0 || host["next_upgrade"] != nil {
		t.Errorf("host state: %v", host)
	}
}

func TestUpgradeGuest(t *testing.T) {
	e := newEnv(t, nil)
	e.command("lab1_web", ha.CmdUpgrade)
	st := e.state(t, "lab1_web")
	if st["last_run_result"] != "upgraded" || st["updates"] != 0.0 || st["image_updates"] != 0.0 {
		t.Fatalf("after upgrade: %v", st)
	}
	run := st["last_run"].(map[string]any)
	var names []string
	for _, s := range run["steps"].([]any) {
		step := s.(map[string]any)
		names = append(names, step["name"].(string))
		if step["ok"] != true {
			t.Errorf("step failed: %v", step)
		}
	}
	want := "snapshot,packages,package cleanup,docker containers,docker compose app,docker image prune,restart"
	if strings.Join(names, ",") != want {
		t.Errorf("steps\n got %s\nwant %s", strings.Join(names, ","), want)
	}
	if len(e.incus.Restarts) != 1 || e.incus.Restarts[0] != "default/web" {
		t.Errorf("restarts: %v", e.incus.Restarts)
	}
	snaps, _ := e.incus.Snapshots(context.Background(), guest.Guest{Name: "web", Project: "default"})
	if len(snaps) != 1 || !strings.HasPrefix(snaps[0].Name, SnapshotPrefix) {
		t.Errorf("snapshots: %v", snaps)
	}
	if !e.incus.FakeRunner("default/web").Ran("docker compose -p app --project-directory /root/app -f /root/app/compose.yaml up -d --remove-orphans") {
		t.Error("compose project not recreated")
	}
	if e.host.Ran("--force-confdef") {
		t.Error("host was upgraded by a guest command")
	}

	// A second upgrade changes nothing.
	e.command("lab1_web", ha.CmdUpgrade)
	if st := e.state(t, "lab1_web"); st["last_run_result"] != "up to date" {
		t.Errorf("second run: %v", st["last_run_result"])
	}
}

func TestUpgradeOnlyPackagesOrContainers(t *testing.T) {
	e := newEnv(t, nil)
	e.command("lab1_web", ha.CmdUpgradeContainers)
	if e.web.packageUpgradeRuns != 0 || !e.web.imagesUpgraded {
		t.Errorf("containers only: packages=%d images=%v", e.web.packageUpgradeRuns, e.web.imagesUpgraded)
	}
	e2 := newEnv(t, nil)
	e2.command("lab1_web", ha.CmdUpgradePackages)
	if e2.web.packageUpgradeRuns != 1 || e2.web.imagesUpgraded {
		t.Errorf("packages only: packages=%d images=%v", e2.web.packageUpgradeRuns, e2.web.imagesUpgraded)
	}
}

func TestSnapshotFailureAbortsUpgrade(t *testing.T) {
	e := newEnv(t, nil)
	e.incus.SnapshotErr = errors.New("pool full")
	e.command("lab1_web", ha.CmdUpgrade)
	st := e.state(t, "lab1_web")
	if st["last_run_result"] != "failed" || e.web.packageUpgradeRuns != 0 {
		t.Fatalf("upgrade must not run without snapshot: %v runs=%d", st["last_run_result"], e.web.packageUpgradeRuns)
	}
}

func TestUnsupportedOSIsNotAFailure(t *testing.T) {
	e := newEnv(t, nil)
	e.command("lab1_alp", ha.CmdUpgrade)
	if st := e.state(t, "lab1_alp"); st["last_run_result"] != "up to date" {
		t.Fatalf("alpine: %v", st["last_run"])
	}
}

func TestPermissions(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {
		c.Permissions = config.Permissions{Upgrade: false, Reboot: false, Restore: false}
	})
	e.command("lab1_web", ha.CmdUpgrade)
	e.command("lab1_web", ha.CmdReboot)
	e.command("lab1_web", ha.CmdRestore)
	e.command("lab1", ha.CmdUpgradeAll)
	e.command("lab1_web", "rm -rf /")
	e.command("nope", ha.CmdCheck)
	if e.web.packageUpgradeRuns != 0 || len(e.incus.Restarts) != 0 || len(e.incus.Restores) != 0 || e.hostM.packageUpgradeRuns != 0 {
		t.Fatal("a forbidden command ran")
	}
	web := components(e.discovery(t, "lab1_web"))
	for _, key := range []string{"upgrade", "reboot", "restore_snapshot"} {
		if _, ok := web[key]; ok {
			t.Errorf("%s button published although not permitted", key)
		}
	}
	if _, ok := web["system_update"].(map[string]any)["command_topic"]; ok {
		t.Error("update entity must not be installable without upgrade permission")
	}
}

func TestUpgradeAllOrderAndHostReboot(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Upgrade.RebootHost = true })
	e.command("lab1_web", ha.CmdUpgradeAll)
	if e.hostM.packageUpgradeRuns != 0 {
		t.Fatal("upgrade_all accepted on a guest")
	}
	e.command("lab1", ha.CmdUpgradeAll)
	if e.hostM.packageUpgradeRuns != 1 || e.web.packageUpgradeRuns != 1 || !e.fed.upgraded {
		t.Fatalf("not everything upgraded: host=%d web=%d fed=%v", e.hostM.packageUpgradeRuns, e.web.packageUpgradeRuns, e.fed.upgraded)
	}
	if e.reboots != 1 {
		t.Errorf("host reboots: %d", e.reboots)
	}
	// The host is upgraded before any guest.
	var order []string
	for _, m := range e.mqtt.Log {
		if strings.HasSuffix(m.Topic, "/state") && strings.Contains(string(m.Payload), `"operation":"upgrade"`) {
			order = append(order, strings.Split(m.Topic, "/")[2])
		}
	}
	if len(order) == 0 || order[0] != "lab1" {
		t.Errorf("upgrade order: %v", order)
	}
}

func TestHostRebootNotAllowedByDefault(t *testing.T) {
	e := newEnv(t, nil)
	e.command("lab1", ha.CmdUpgrade)
	st := e.state(t, "lab1")
	if e.reboots != 0 || st["reboot_required"] != true {
		t.Fatalf("reboots=%d state=%v", e.reboots, st["reboot_required"])
	}
}

func TestGuestRestartDisabled(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Upgrade.RestartGuests = false })
	e.command("lab1_web", ha.CmdUpgrade)
	if len(e.incus.Restarts) != 0 || e.state(t, "lab1_web")["reboot_required"] != true {
		t.Fatal("guest restarted although restart_guests is false")
	}
}

func TestRestoreSnapshot(t *testing.T) {
	e := newEnv(t, nil)
	e.command("lab1_web", ha.CmdRestore)
	if st := e.state(t, "lab1_web"); st["last_run_result"] != "failed" {
		t.Fatalf("restore without snapshot must fail: %v", st["last_run"])
	}
	e.command("lab1_web", ha.CmdUpgrade)
	e.command("lab1_web", ha.CmdRestore)
	if len(e.incus.Restores) != 1 || !strings.HasPrefix(e.incus.Restores[0], "default/web@"+SnapshotPrefix) {
		t.Fatalf("restores: %v", e.incus.Restores)
	}
	e.command("lab1", ha.CmdRestore)
	if len(e.incus.Restores) != 1 {
		t.Fatal("host cannot be restored")
	}
}

func TestGuestLifecycle(t *testing.T) {
	e := newEnv(t, nil)
	e.incus.SetGuests(
		guest.Guest{Name: "web", Project: "default", Type: "container", Status: "Running"},
		guest.Guest{Name: "old", Project: "default", Type: "container", Status: "Running"},
	)
	deadline := time.Now().Add(5 * time.Second)
	go func() {
		_ = e.incus.WatchLifecycle(context.Background(), func() { e.agent.enqueue(op{kind: "sync"}) })
	}()
	for time.Now().Before(deadline) {
		e.agent.WaitIdle()
		if _, ok := e.mqtt.Retained(e.agent.Topics().Config("lab1_alp")); !ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := e.mqtt.Retained(e.agent.Topics().Config("lab1_alp")); ok {
		t.Fatal("removed guest still announced")
	}
	if _, ok := e.mqtt.Retained(e.agent.Topics().State("lab1_dev_fed")); ok {
		t.Fatal("removed guest state still retained")
	}
	if !e.incus.FakeRunner("default/old").Ran("os-release") {
		t.Fatal("started guest was not checked")
	}
}

func TestAutoUpgradeSwitchPersists(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Upgrade.Schedule = "30 3 * * 0" })
	host := e.state(t, "lab1")
	if host["auto_upgrade"] != true || host["next_upgrade"] != "2026-10-25T03:30:00Z" {
		t.Fatalf("host: %v %v", host["auto_upgrade"], host["next_upgrade"])
	}
	if _, ok := components(e.discovery(t, "lab1"))["auto_upgrade"]; !ok {
		t.Fatal("switch missing")
	}
	_ = e.mqtt.Publish(e.agent.Topics().AutoUpgrade(), false, []byte("OFF"))
	host = e.state(t, "lab1")
	if host["auto_upgrade"] != false || host["next_upgrade"] != nil {
		t.Fatalf("after OFF: %v %v", host["auto_upgrade"], host["next_upgrade"])
	}
	data, err := os.ReadFile(e.cfg.StateFile)
	if err != nil || !strings.Contains(string(data), `"auto_upgrade": false`) {
		t.Fatalf("state file: %s %v", data, err)
	}
	st, _ := loadState(e.cfg.StateFile)
	if st.AutoUpgrade == nil || *st.AutoUpgrade {
		t.Fatal("auto upgrade not persisted")
	}
}

func TestStateSurvivesRestartAndCleansStaleDevices(t *testing.T) {
	e := newEnv(t, nil)
	e.command("lab1_web", ha.CmdUpgrade)
	e.cancel()

	// Same state file and broker, but "alp" was deleted while the agent was down.
	mem := e.mqtt
	inc := guest.NewFake(guest.Guest{Name: "web", Project: "default", Type: "container", Status: "Stopped"})
	a, err := New(Options{Config: e.cfg, MQTT: mem, Providers: []guest.Provider{inc}, Local: e.host, Hostname: "lab1", Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	a.WaitIdle()
	if _, ok := mem.Retained(a.Topics().Config("lab1_alp")); ok {
		t.Error("stale guest from before the restart still announced")
	}
	payload, _ := mem.Retained(a.Topics().State("lab1_web"))
	if !strings.Contains(string(payload), `"last_run_result":"upgraded"`) {
		t.Errorf("last run not restored: %s", payload)
	}
}

func TestHomeAssistantBirthRepublishes(t *testing.T) {
	e := newEnv(t, nil)
	before := len(e.mqtt.Log)
	_ = e.mqtt.Publish("homeassistant/status", false, []byte("online"))
	n := 0
	for _, m := range e.mqtt.Log[before:] {
		if strings.HasSuffix(m.Topic, "/config") {
			n++
		}
	}
	if n != 5 {
		t.Fatalf("expected 5 discovery messages after HA birth, got %d", n)
	}
}

func TestHostNotManaged(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Host.Manage = false })
	if e.host.Ran("os-release") {
		t.Error("host checked although host.manage is false")
	}
	e.command("lab1", ha.CmdUpgrade)
	if e.hostM.packageUpgradeRuns != 0 {
		t.Error("host upgraded although host.manage is false")
	}
	e.command("lab1", ha.CmdUpgradeAll)
	if !e.fed.upgraded {
		t.Error("guests must still be upgraded by upgrade_all")
	}
}
