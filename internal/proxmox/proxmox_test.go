package proxmox

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lvlie/linux-ha-agent/internal/guest"
	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// Output in the format pvesh documents for Proxmox VE 8/9. LXC VMIDs come back as
// strings, QEMU VMIDs as numbers.
const (
	lxcList = `[
 {"cpu":0.01,"cpus":2,"maxmem":2147483648,"name":"web","pid":1234,"status":"running","type":"lxc","uptime":3600,"vmid":"101"},
 {"name":"media","status":"stopped","type":"lxc","vmid":"104"},
 {"name":"sonarr","status":"running","type":"lxc","vmid":"105"},
 {"name":"debian-tmpl","status":"stopped","template":1,"type":"lxc","vmid":"900"}
]`
	qemuList = `[
 {"cpus":4,"name":"win11","status":"running","vmid":200},
 {"cpus":2,"name":"docker-vm","status":"running","vmid":201},
 {"name":"stopped-vm","status":"stopped","vmid":202},
 {"name":"ubuntu-tmpl","status":"stopped","template":1,"vmid":9000}
]`
	snapshotList = `[
 {"name":"ha-agent-20261001-030000","snaptime":1759287600,"description":"linux-ha-agent pre-upgrade snapshot; expires 2026-10-08T03:00:00Z"},
 {"name":"ha-agent-20261019-030000","snaptime":1760842800,"description":"linux-ha-agent pre-upgrade snapshot; expires 2026-10-26T03:00:00Z"},
 {"name":"before-migration","snaptime":1700000000,"description":"manual"},
 {"name":"current","running":1,"description":"You are here!","parent":"ha-agent-20261019-030000"}
]`
)

func node(t *testing.T, f *runner.Fake) *Provider {
	t.Helper()
	f.On("hostname", runner.Result{Stdout: "pve1.lan\n"})
	p, err := New(context.Background(), f, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p.now = func() time.Time { return time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC) }
	p.execPoll = time.Millisecond
	return p
}

func TestNewUsesShortHostname(t *testing.T) {
	p := node(t, &runner.Fake{})
	if p.Node() != "pve1" || p.Platform() != "proxmox" {
		t.Fatalf("node %q platform %q", p.Node(), p.Platform())
	}
	if _, err := New(context.Background(), &runner.Fake{}, "bad/node", time.Minute); err == nil {
		t.Fatal("expected invalid node error")
	}
}

func TestGuests(t *testing.T) {
	f := (&runner.Fake{}).
		On("pvesh get /nodes/pve1/lxc --output-format json", runner.Result{Stdout: lxcList}).
		On("pvesh get /nodes/pve1/qemu --output-format json", runner.Result{Stdout: qemuList}).
		On("cat /etc/pve/nodes/pve1/lxc/105.conf", runner.Result{Stdout: sonarrConf}).
		On("cat /etc/pve/nodes/pve1/lxc/", runner.Result{Stdout: "arch: amd64\nostype: ubuntu\n"}).
		On("pvesh get /nodes/pve1/qemu/200/config", runner.Result{Stdout: `{"agent":"0","ostype":"win11"}`}).
		On("pvesh get /nodes/pve1/qemu/201/config", runner.Result{Stdout: `{"agent":"enabled=1,fstrim_cloned_disks=1"}`})
	guests, err := node(t, f).Guests(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, g := range guests {
		got = append(got, strings.Join([]string{g.ID, g.Name, g.Type, g.Status, g.Platform, g.Unmanageable}, "|"))
	}
	want := []string{
		"201|docker-vm|virtual-machine|running|proxmox|",
		"104|media|container|stopped|proxmox|",
		"105|sonarr|container|running|proxmox|",
		"202|stopped-vm|virtual-machine|stopped|proxmox|",
		"101|web|container|running|proxmox|",
		"200|win11|virtual-machine|running|proxmox|the QEMU guest agent is not enabled for this VM",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("guests\n got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if f.Ran("qemu/202/config") {
		t.Error("stopped VMs need no agent check")
	}
	if guests[4].Key() != "101" {
		t.Errorf("key %q", guests[4].Key())
	}
}

func TestGuestsFindOCIApps(t *testing.T) {
	f := (&runner.Fake{}).
		On("pvesh get /nodes/pve1/lxc --output-format json", runner.Result{Stdout: lxcList}).
		On("pvesh get /nodes/pve1/qemu --output-format json", runner.Result{Stdout: "[]"}).
		On("cat /etc/pve/nodes/pve1/lxc/105.conf", runner.Result{Stdout: sonarrConf}).
		On("cat /etc/pve/nodes/pve1/lxc/", runner.Result{Stdout: "arch: amd64\nostype: ubuntu\n"})
	guests, err := node(t, f).Guests(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range guests {
		switch {
		case g.ID == "105":
			if g.App == nil || *g.App != (guest.App{Image: "ghcr.io/linuxserver/sonarr:latest", ByDigest: true}) {
				t.Errorf("sonarr app: %+v", g.App)
			}
		case g.App != nil:
			t.Errorf("%s is a system container: %+v", g.Name, g.App)
		}
	}
}

// sonarrConf is the configuration file of a container created from an OCI
// image, with the image in its notes; a snapshot section follows.
const sonarrConf = `#Sonarr, created from an OCI image.
#
#image%3A ` + "`ghcr.io/linuxserver/sonarr%3Alatest`" + `
arch: amd64
cmode: console
entrypoint: /init
env: PUID=1000
hostname: sonarr
ostype: unmanaged
rootfs: local-lvm:vm-105-disk-0,size=8G

[before-update]
#image%3A ghcr.io/linuxserver/sonarr%3A4
entrypoint: /old-init
`

func TestApp(t *testing.T) {
	const pinned = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	noImage := `add a line "image: <reference>" to the notes of the container`
	for _, tc := range []struct {
		entrypoint, notes string
		want              *guest.App
	}{
		{"", "image: nginx", nil},
		{"/sbin/init", "image: nginx", nil},
		{"/init", "", &guest.App{ByDigest: true, NoImage: noImage}},
		{"/init", "Image:   docker.io/library/nginx:1.27  ", &guest.App{Image: "docker.io/library/nginx:1.27", ByDigest: true}},
		{"/init", "image: nginx:1.27@" + pinned, &guest.App{Image: "nginx:1.27", Fingerprint: pinned, ByDigest: true}},
		{"/init", "image: nginx@" + pinned, &guest.App{Image: "nginx@" + pinned, Fingerprint: pinned, ByDigest: true}},
		{"/init", "image: two words\nimage: nginx", &guest.App{Image: "nginx", ByDigest: true}},
	} {
		got := App(tc.entrypoint, tc.notes)
		if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
			t.Errorf("App(%q, %q) = %+v, want %+v", tc.entrypoint, tc.notes, got, tc.want)
		}
	}
}

func TestParseConfig(t *testing.T) {
	entrypoint, notes := ParseConfig(sonarrConf)
	if entrypoint != "/init" || notes != "Sonarr, created from an OCI image.\n\nimage: `ghcr.io/linuxserver/sonarr:latest`\n" {
		t.Errorf("entrypoint %q, notes %q", entrypoint, notes)
	}
}

func TestAgentEnabled(t *testing.T) {
	for value, want := range map[string]bool{
		"1": true, "1,fstrim_cloned_disks=1": true, "enabled=1,type=virtio": true,
		"0": false, "": false, "enabled=0": false, "fstrim_cloned_disks=1": false,
	} {
		if got := AgentEnabled(value); got != want {
			t.Errorf("AgentEnabled(%q) = %v", value, got)
		}
	}
}

func TestContainerRunner(t *testing.T) {
	f := (&runner.Fake{}).On("pct exec 101 -- env", runner.Result{Stdout: "ID=debian\n", ExitCode: 3})
	p := node(t, f)
	res, err := p.Runner(guest.Guest{ID: "101", Type: guest.Container}).Run(context.Background(),
		runner.Command("cat", "/etc/os-release").WithEnv("DEBIAN_FRONTEND", "noninteractive"))
	if err != nil || res.ExitCode != 3 || res.Stdout != "ID=debian\n" {
		t.Fatalf("got %+v %v", res, err)
	}
	line := f.Calls[len(f.Calls)-1].String()
	for _, want := range []string{"pct exec 101 -- env ", "DEBIAN_FRONTEND=noninteractive", "LC_ALL=C", "PATH=/run/wrappers/bin:", " cat /etc/os-release"} {
		if !strings.Contains(line, want) {
			t.Errorf("command %q misses %q", line, want)
		}
	}
}

func TestVMRunnerPollsUntilExit(t *testing.T) {
	var mu sync.Mutex
	polls := 0
	f := (&runner.Fake{}).
		On("qm guest exec 201 --timeout 0 -- env", runner.Result{Stdout: `{"pid":4242}`}).
		OnFunc("qm guest exec-status 201 4242", func(runner.Cmd) runner.Result {
			mu.Lock()
			defer mu.Unlock()
			polls++
			if polls < 3 {
				return runner.Result{Stdout: `{"exited":0}`}
			}
			return runner.Result{Stdout: `{"exited":1,"exitcode":100,"out-data":"some output\n","err-data":"E: oops\n"}`}
		})
	res, err := node(t, f).Runner(guest.Guest{ID: "201", Type: guest.VirtualMachine}).Run(context.Background(), runner.Command("apt-get", "update"))
	if err != nil || res.ExitCode != 100 || res.Stdout != "some output\n" || res.Stderr != "E: oops\n" || polls != 3 {
		t.Fatalf("got %+v %v after %d polls", res, err, polls)
	}
}

func TestVMRunnerErrors(t *testing.T) {
	down := (&runner.Fake{}).On("qm guest exec", runner.Result{ExitCode: 255, Stderr: "QEMU guest agent is not running\n"})
	_, err := node(t, down).Runner(guest.Guest{ID: "201", Type: guest.VirtualMachine}).Run(context.Background(), runner.Command("true"))
	if err == nil || !strings.Contains(err.Error(), "QEMU guest agent is not running") {
		t.Fatalf("got %v", err)
	}

	hang := (&runner.Fake{}).
		On("qm guest exec 201 --timeout 0", runner.Result{Stdout: `{"pid":1}`}).
		On("exec-status", runner.Result{Stdout: `{"exited":0}`})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = node(t, hang).Runner(guest.Guest{ID: "201", Type: guest.VirtualMachine}).Run(ctx, runner.Command("sleep", "1000"))
	if err == nil {
		t.Fatal("expected context error")
	}
}

func TestSnapshotsCreatePruneRestore(t *testing.T) {
	f := (&runner.Fake{}).
		On("pct snapshot 101 ha-agent-20261020-030000", runner.Result{}).
		On("pvesh get /nodes/pve1/lxc/101/snapshot", runner.Result{Stdout: snapshotList}).
		On("pct delsnapshot", runner.Result{}).
		On("pct rollback", runner.Result{}).
		On("qm rollback", runner.Result{}).
		On("qm reboot 201 --timeout 120", runner.Result{})
	p := node(t, f)
	ct := guest.Guest{ID: "101", Name: "web", Type: guest.Container, Status: "running"}

	if err := p.CreateSnapshot(context.Background(), ct, "ha-agent-20261020-030000", p.now().Add(7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !f.Ran("--description linux-ha-agent pre-upgrade snapshot; expires 2026-10-27T03:00:00Z") {
		t.Errorf("snapshot description missing: %v", f.Calls)
	}
	var deleted []string
	for _, c := range f.Calls {
		if c.Args[0] == "pct" && c.Args[1] == "delsnapshot" {
			deleted = append(deleted, c.Args[3])
		}
	}
	if len(deleted) != 1 || deleted[0] != "ha-agent-20261001-030000" {
		t.Errorf("only the expired agent snapshot may be pruned, deleted %v", deleted)
	}

	snaps, err := p.Snapshots(context.Background(), ct)
	if err != nil || len(snaps) != 3 || snaps[0].Name != "ha-agent-20261019-030000" || snaps[2].Name != "before-migration" {
		t.Fatalf("snapshots %+v %v", snaps, err)
	}

	if err := p.RestoreSnapshot(context.Background(), ct, "ha-agent-20261019-030000"); err != nil {
		t.Fatal(err)
	}
	if !f.Ran("pct rollback 101 ha-agent-20261019-030000 --start 1") {
		t.Error("running container must be started after rollback")
	}
	vm := guest.Guest{ID: "201", Type: guest.VirtualMachine, Status: "stopped"}
	if err := p.RestoreSnapshot(context.Background(), vm, "x"); err != nil {
		t.Fatal(err)
	}
	if f.Ran("qm rollback 201 x --start") {
		t.Error("stopped VM must stay stopped after rollback")
	}
	if err := p.Restart(context.Background(), vm, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestWatchLifecyclePolls(t *testing.T) {
	var mu sync.Mutex
	status := "running"
	f := (&runner.Fake{}).
		OnFunc("pvesh get /nodes/pve1/lxc --output-format json", func(runner.Cmd) runner.Result {
			mu.Lock()
			defer mu.Unlock()
			return runner.Result{Stdout: `[{"name":"web","status":"` + status + `","vmid":"101"}]`}
		}).
		On("pvesh get /nodes/pve1/qemu --output-format json", runner.Result{Stdout: "[]"}).
		On("cat /etc/pve/nodes/pve1/lxc/101.conf", runner.Result{Stdout: "ostype: ubuntu\n"})
	p := node(t, f)
	p.poll = 5 * time.Millisecond
	changes := make(chan struct{}, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.WatchLifecycle(ctx, func() { changes <- struct{}{} }) }()
	time.Sleep(30 * time.Millisecond)
	select {
	case <-changes:
		t.Fatal("no change must not trigger")
	default:
	}
	mu.Lock()
	status = "stopped"
	mu.Unlock()
	select {
	case <-changes:
	case <-time.After(2 * time.Second):
		t.Fatal("state change not noticed")
	}
}

func TestDetect(t *testing.T) {
	pve := (&runner.Fake{}).On("test -d /etc/pve", runner.Result{}).On(`command -v "$1"`, runner.Result{})
	if !Detect(context.Background(), pve) {
		t.Error("proxmox node not detected")
	}
	other := (&runner.Fake{}).On("test -d /etc/pve", runner.Result{ExitCode: 1})
	if Detect(context.Background(), other) {
		t.Error("detected proxmox on a plain machine")
	}
}
