package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lvlie/linux-ha-agent/internal/config"
	"github.com/lvlie/linux-ha-agent/internal/guest"
	"github.com/lvlie/linux-ha-agent/internal/ha"
	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// Fingerprint answers "fp:<ref>" entries of the test registry.
func (r registry) Fingerprint(_ context.Context, ref, _ string) (string, error) {
	if fp, ok := r["fp:"+ref]; ok {
		return fp, nil
	}
	return "", errors.New("not found")
}

const (
	oldFP = "1111111111111111111111111111111111111111111111111111111111111111"
	newFP = "2222222222222222222222222222222222222222222222222222222222222222"
)

func appGuest(name string, app guest.App) guest.Guest {
	if app.Fingerprint == "" {
		app.Fingerprint = oldFP
	}
	app.Architecture = "amd64"
	return guest.Guest{Platform: "incus", Name: name, Project: "default", Type: "container", Status: "Running", App: &app}
}

// withApps adds application containers to the default guests.
func withApps(apps ...guest.Guest) func(*env) {
	return func(e *env) {
		guests, _ := e.incus.Guests(context.Background())
		e.incus.SetGuests(append(guests, apps...)...)
	}
}

// replaceImage makes the provider report a new fingerprint for a guest.
func replaceImage(e *env, key, fp string) {
	guests, _ := e.incus.Guests(context.Background())
	for i, g := range guests {
		if g.Key() == key {
			app := *g.App
			app.Fingerprint = fp
			guests[i].App = &app
		}
	}
	e.incus.SetGuests(guests...)
}

func TestOCIAppUpdate(t *testing.T) {
	e := newEnvWith(t, nil, withApps(appGuest("nginx", guest.App{Image: "docker.io/library/nginx:1.27"})))
	e.agent.registry = registry{"nginx:1.27": "sha256:digest-new", "fp:docker.io/library/nginx:1.27": newFP}
	e.command("lab1_nginx", ha.CmdCheck)

	st := e.state(t, "lab1_nginx")
	u := st["app_update"].(map[string]any)
	if st["updates_available"] != true || u["installed_version"] != oldFP[:12] || u["latest_version"] != newFP[:12] || st["platform"] != "Incus OCI container" {
		t.Fatalf("state: %v", st)
	}
	if e.state(t, "lab1")["image_updates_total"].(float64) < 1 {
		t.Error("host total misses the app image")
	}
	cmps := components(e.discovery(t, "lab1_nginx"))
	if c, ok := cmps["app_update"].(map[string]any); !ok || c["payload_install"] != ha.CmdUpgrade {
		t.Fatalf("app update entity: %v", cmps["app_update"])
	}
	for _, key := range []string{"system_update", "kernel", "cpu", "os", "updates"} {
		if _, ok := cmps[key]; ok {
			t.Errorf("app container got machine entity %s", key)
		}
	}
	if _, ok := cmps["restore_snapshot"]; ok {
		t.Error("app containers are rebuilt, which Incus refuses with snapshots: no restore")
	}
	for _, key := range []string{"upgrade", "reboot", "running", "check"} {
		if _, ok := cmps[key]; !ok {
			t.Errorf("app container misses %s", key)
		}
	}
	if calls := e.incus.FakeRunner("default/nginx").Calls; len(calls) != 0 {
		t.Errorf("commands ran inside the app container: %v", calls)
	}

	e.incus.OnRebuild = func(g guest.Guest) { replaceImage(e, g.Key(), newFP) }
	e.command("lab1_nginx", ha.CmdUpgrade)
	if len(e.incus.Rebuilds) != 1 || e.incus.Rebuilds[0] != "default/nginx" {
		t.Fatalf("rebuilds: %v", e.incus.Rebuilds)
	}
	if snaps, _ := e.incus.Snapshots(context.Background(), guest.Guest{Name: "nginx", Project: "default"}); len(snaps) != 0 {
		t.Errorf("snapshot taken, which makes the rebuild fail: %v", snaps)
	}
	st = e.state(t, "lab1_nginx")
	last := st["last_run"].(map[string]any)
	if st["updates_available"] != false || last["success"] != true || last["changed"] != true {
		t.Fatalf("after upgrade: %v %v", st["updates_available"], last)
	}

	// Up to date: no rebuild.
	e.command("lab1_nginx", ha.CmdUpgrade)
	if len(e.incus.Rebuilds) != 1 {
		t.Error("rebuilt although up to date")
	}
}

func TestOCIAppWithSnapshotsIsNotRebuilt(t *testing.T) {
	e := newEnvWith(t, nil, withApps(appGuest("nginx", guest.App{Image: "docker.io/library/nginx:1.27"})))
	e.agent.registry = registry{"fp:docker.io/library/nginx:1.27": newFP}
	nginx := guest.Guest{Name: "nginx", Project: "default"}
	_ = e.incus.CreateSnapshot(context.Background(), nginx, "mine", time.Time{})
	e.command("lab1_nginx", ha.CmdUpgrade)
	if len(e.incus.Rebuilds) != 0 {
		t.Fatal("rebuilt an instance with snapshots")
	}
	last := e.state(t, "lab1_nginx")["last_run"].(map[string]any)
	steps := last["steps"].([]any)
	if last["success"] != false || !strings.Contains(steps[len(steps)-1].(map[string]any)["detail"].(string), "snapshots") {
		t.Errorf("last run: %v", last)
	}
	if snaps, _ := e.incus.Snapshots(context.Background(), nginx); len(snaps) != 1 {
		t.Error("the user's snapshot must be kept")
	}
}

func TestOCIAppUpgradeAll(t *testing.T) {
	e := newEnvWith(t, nil, withApps(appGuest("nginx", guest.App{Image: "docker.io/library/nginx:1.27"})))
	e.agent.registry = registry{"nginx:1.27": "sha256:digest-new", "fp:docker.io/library/nginx:1.27": newFP}
	e.command("lab1", ha.CmdUpgradeAll)
	if len(e.incus.Rebuilds) != 1 {
		t.Fatalf("app not upgraded with the host and guests: %v", e.incus.Rebuilds)
	}
}

func TestIncusComposeApp(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "web")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {app: {image: ghcr.io/me/app:v2}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var e *env
	e = newEnvWith(t, func(c *config.Config) {
		c.Incus.ComposeDirs = []string{filepath.Dir(dir)}
		c.Permissions.Restore = true
	}, func(env *env) {
		withApps(
			appGuest("web-app-1", guest.App{Image: "ghcr.io/me/app:v2", ComposeProject: "web", ComposeService: "app"}),
			appGuest("other-db-1", guest.App{Image: "docker.io/library/postgres:17", ComposeProject: "other", ComposeService: "db"}),
		)(env)
		env.host.On("sh incus-compose", runner.Result{}).
			OnFunc("incus-compose up --detach --pull always --no-deps app", func(runner.Cmd) runner.Result {
				replaceImage(e, "default/web-app-1", newFP)
				return runner.Result{}
			})
	})
	e.agent.registry = registry{"fp:ghcr.io/me/app:v2": newFP, "fp:docker.io/library/postgres:17": newFP}
	e.command("lab1_web_app_1", ha.CmdCheck)
	e.command("lab1_other_db_1", ha.CmdCheck)

	web := components(e.discovery(t, "lab1_web_app_1"))
	if c := web["app_update"].(map[string]any); c["payload_install"] != ha.CmdUpgrade {
		t.Errorf("compose app not installable: %v", c)
	}
	if _, ok := web["restore_snapshot"]; ok {
		t.Error("incus-compose recreates instances; restoring a snapshot is not possible")
	}
	if e.state(t, "lab1_web_app_1")["platform"] != "incus-compose service" {
		t.Error("platform")
	}
	other := components(e.discovery(t, "lab1_other_db_1"))
	if _, ok := other["app_update"].(map[string]any)["payload_install"]; ok {
		t.Error("compose project without a known directory must not be installable")
	}
	if _, ok := other["upgrade"]; ok {
		t.Error("upgrade button for a project that cannot be found")
	}

	e.command("lab1_web_app_1", ha.CmdUpgrade)
	if !e.host.Ran("incus-compose up --detach --pull always --no-deps app") {
		t.Fatal("incus-compose did not run")
	}
	if len(e.incus.Rebuilds) != 0 {
		t.Error("an incus-compose instance must not be rebuilt behind its back")
	}
	if snaps, _ := e.incus.Snapshots(context.Background(), guest.Guest{Name: "web-app-1", Project: "default"}); len(snaps) != 0 {
		t.Error("snapshot taken although incus-compose recreates the instance")
	}
	last := e.state(t, "lab1_web_app_1")["last_run"].(map[string]any)
	if last["success"] != true || last["changed"] != true {
		t.Fatalf("last run: %v", last)
	}

	e.command("lab1_other_db_1", ha.CmdUpgrade)
	if e.state(t, "lab1_other_db_1")["last_run"] != nil {
		t.Error("upgrade accepted although the project cannot be found")
	}
}

func lastRun(t *testing.T, e *env, id string) map[string]any {
	t.Helper()
	last, ok := e.state(t, id)["last_run"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no last run", id)
	}
	return last
}

func TestStoppedOCIAppIsUpdated(t *testing.T) {
	stopped := appGuest("nginx", guest.App{Image: "docker.io/library/nginx:1.27"})
	stopped.Status = "Stopped"
	e := newEnvWith(t, nil, withApps(stopped))
	// Checked when found, although stopped: nothing runs inside it.
	e.agent.mu.Lock()
	checked := e.agent.devices["lab1_nginx"].Check != nil
	e.agent.mu.Unlock()
	if !checked {
		t.Fatal("stopped app not checked when discovered")
	}
	e.agent.registry = registry{"fp:docker.io/library/nginx:1.27": newFP}
	e.command("lab1_nginx", ha.CmdCheck)
	if e.state(t, "lab1_nginx")["updates_available"] != true {
		t.Fatal("no update found for the stopped app")
	}
	if c := components(e.discovery(t, "lab1_nginx"))["app_update"].(map[string]any); c["payload_install"] != ha.CmdUpgrade {
		t.Fatalf("stopped app not installable: %v", c)
	}
	e.incus.OnRebuild = func(g guest.Guest) { replaceImage(e, g.Key(), newFP) }
	e.command("lab1_nginx", ha.CmdUpgrade)
	if len(e.incus.Rebuilds) != 1 {
		t.Fatalf("stopped app not rebuilt: %v", lastRun(t, e, "lab1_nginx"))
	}
	if last := lastRun(t, e, "lab1_nginx"); last["success"] != true {
		t.Errorf("last run: %v", last)
	}

	replaceImage(e, "default/nginx", oldFP)
	e.agent.syncGuests(context.Background())
	e.command("lab1", ha.CmdUpgradeAll)
	if len(e.incus.Rebuilds) != 2 {
		t.Error("upgrade_all skipped the stopped app")
	}
}

func TestStoppedComposeAppIsNotStarted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "web")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {app: {}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stopped := appGuest("web-app-1", guest.App{Image: "ghcr.io/me/app:v2", ComposeProject: "web", ComposeService: "app"})
	stopped.Status = "Stopped"
	e := newEnvWith(t, func(c *config.Config) { c.Incus.ComposeDirs = []string{filepath.Dir(dir)} }, func(env *env) {
		withApps(stopped)(env)
		env.host.On("sh incus-compose", runner.Result{}).On("incus-compose up", runner.Result{})
	})
	e.agent.registry = registry{"fp:ghcr.io/me/app:v2": newFP}
	if _, ok := components(e.discovery(t, "lab1_web_app_1"))["upgrade"]; ok {
		t.Error("upgrade button for a stopped compose service")
	}
	e.command("lab1_web_app_1", ha.CmdUpgrade)
	e.command("lab1", ha.CmdUpgradeAll)
	if e.host.Ran("incus-compose up") {
		t.Error("incus-compose started a stopped service")
	}
}

func TestComposeAppInstallableBeforeCheck(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "web")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {app: {}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newEnvWith(t, func(c *config.Config) { c.Incus.ComposeDirs = []string{filepath.Dir(dir)} },
		withApps(appGuest("web-app-1", guest.App{Image: "ghcr.io/me/app:v2", ComposeProject: "web", ComposeService: "app"})))
	e.agent.mu.Lock()
	d := e.agent.devices["lab1_web_app_1"]
	d.Check = nil
	installable := e.agent.appInstallable(d)
	e.agent.mu.Unlock()
	if !installable {
		t.Error("compose app not installable until checked")
	}
}

func TestOCIAppRebuildWhenRegistryUnknown(t *testing.T) {
	e := newEnvWith(t, nil, withApps(appGuest("nginx", guest.App{Image: "docker.io/library/nginx:1.27"})))
	e.agent.registry = registry{} // the registry cannot be asked

	// Scheduled upgrades of all devices do not stop the app on a guess.
	e.command("lab1", ha.CmdUpgradeAll)
	if len(e.incus.Rebuilds) != 0 {
		t.Fatal("rebuilt on upgrade_all although the registry could not be asked")
	}
	// Asked for this app alone, it is rebuilt.
	e.command("lab1_nginx", ha.CmdUpgrade)
	if len(e.incus.Rebuilds) != 1 {
		t.Fatal("explicit upgrade did not rebuild")
	}
}

func TestOCIAppSnapshotListingFails(t *testing.T) {
	e := newEnvWith(t, nil, withApps(appGuest("nginx", guest.App{Image: "docker.io/library/nginx:1.27"})))
	e.agent.registry = registry{"fp:docker.io/library/nginx:1.27": newFP}
	e.incus.SnapshotsErr = errors.New("api down")
	e.command("lab1_nginx", ha.CmdUpgrade)
	if len(e.incus.Rebuilds) != 0 {
		t.Fatal("rebuilt without knowing about snapshots")
	}
	if last := lastRun(t, e, "lab1_nginx"); last["success"] != false {
		t.Errorf("last run: %v", last)
	}
}

func TestOCIAppRejectsPartialUpgrades(t *testing.T) {
	e := newEnvWith(t, nil, withApps(appGuest("nginx", guest.App{Image: "docker.io/library/nginx:1.27"})))
	e.agent.registry = registry{"fp:docker.io/library/nginx:1.27": newFP}
	for _, cmd := range []string{ha.CmdUpgradePackages, ha.CmdUpgradeContainers} {
		e.command("lab1_nginx", cmd)
	}
	if len(e.incus.Rebuilds) != 0 {
		t.Errorf("partial upgrade rebuilt the app: %v", e.incus.Rebuilds)
	}
	if e.state(t, "lab1_nginx")["last_run"] != nil {
		t.Error("partial upgrade accepted")
	}
}

func TestOCIAppUpdatedOutsideTheAgent(t *testing.T) {
	e := newEnvWith(t, nil, withApps(appGuest("nginx", guest.App{Image: "docker.io/library/nginx:1.27"})))
	e.agent.registry = registry{"fp:docker.io/library/nginx:1.27": newFP}
	e.command("lab1_nginx", ha.CmdCheck)
	if e.state(t, "lab1_nginx")["updates_available"] != true {
		t.Fatal("no update found")
	}
	// e.g. `incus-compose up` by hand: the lifecycle sync loads the new image.
	replaceImage(e, "default/nginx", newFP)
	e.agent.syncGuests(context.Background())
	st := e.state(t, "lab1_nginx")
	if st["updates_available"] != false || st["image_updates"].(float64) != 0 {
		t.Errorf("update still pending after the image changed: %v %v", st["updates_available"], st["image_updates"])
	}
}

func TestAppCheckWithoutApp(t *testing.T) {
	e := newEnvWith(t, nil, withApps(appGuest("nginx", guest.App{Image: "docker.io/library/nginx:1.27"})))
	e.agent.mu.Lock()
	d := e.agent.devices["lab1_nginx"]
	g := *d.Guest
	g.App = nil
	d.Guest = &g // replaced after check() decided it is an app
	e.agent.mu.Unlock()
	e.agent.appCheck(context.Background(), d) // must not panic
}

func TestComposeDirIsCached(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	a := &Agent{now: func() time.Time { return now }}
	a.cfg.Incus.ComposeDirs = []string{root}
	if _, ok := a.composeDir("web"); ok {
		t.Fatal("found a missing project")
	}
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "web", "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.composeDir("web"); ok {
		t.Error("directories read again within the cache time")
	}
	now = now.Add(composeCacheTTL)
	if dir, ok := a.composeDir("web"); !ok || dir != filepath.Join(root, "web") {
		t.Errorf("cache not refreshed: %q", dir)
	}
}
