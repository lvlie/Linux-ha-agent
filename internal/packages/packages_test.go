package packages

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lvlie/incus-ha-agent/internal/osinfo"
	"github.com/lvlie/incus-ha-agent/internal/runner"
)

const aptSim = `NOTE: This is only a simulation!
Reading package lists...
Building dependency tree...
The following packages will be upgraded:
  libc6 openssl tzdata
Inst libc6 [2.39-0ubuntu8.3] (2.39-0ubuntu8.4 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])
Inst openssl [3.0.13-0ubuntu3.4] (3.0.13-0ubuntu3.5 Ubuntu:24.04/noble-security [amd64])
Inst tzdata [2025a-0ubuntu0.24.04] (2025b-0ubuntu0.24.04 Ubuntu:24.04/noble-updates [all])
Inst linux-new (6.8.0-60.63 Ubuntu:24.04/noble-updates [amd64])
Conf libc6 (2.39-0ubuntu8.4 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])
`

func TestParseAptSimulation(t *testing.T) {
	updates := ParseAptSimulation(aptSim)
	if len(updates) != 4 {
		t.Fatalf("expected 4 updates, got %+v", updates)
	}
	if updates[0].Name != "libc6" || updates[0].Version != "2.39-0ubuntu8.4" || !updates[0].Security {
		t.Errorf("libc6 parsed wrong: %+v", updates[0])
	}
	if updates[2].Security || updates[2].Source != "Ubuntu:24.04/noble-updates" {
		t.Errorf("tzdata parsed wrong: %+v", updates[2])
	}
	if updates[3].Name != "linux-new" || updates[3].Version != "6.8.0-60.63" {
		t.Errorf("new package parsed wrong: %+v", updates[3])
	}
}

func TestAptCheckAndUpgrade(t *testing.T) {
	f := (&runner.Fake{}).
		On("apt-get -q -o DPkg::Lock::Timeout=600 update", runner.Result{}).
		On("-s dist-upgrade", runner.Result{Stdout: aptSim}).
		On("test -e /var/run/reboot-required", runner.Result{ExitCode: 0}).
		On("-y -o Dpkg::Options::=--force-confdef", runner.Result{Stdout: "4 upgraded, 1 newly installed, 0 to remove and 0 not upgraded.\n"}).
		On("autoremove --purge", runner.Result{}).
		On(" clean", runner.Result{})
	m, err := For(osinfo.Info{Family: osinfo.FamilyDebian}, f, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := m.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Updates) != 4 || st.Security != 2 || !st.RebootRequired || st.Manager != "apt" {
		t.Fatalf("unexpected status %+v", st)
	}
	res, err := m.Upgrade(ctx)
	if err != nil || !res.Changed || res.Summary != "4 upgraded, 1 newly installed, 0 removed" {
		t.Fatalf("unexpected result %+v %v", res, err)
	}
	if err := m.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Calls {
		if c.Args[0] == "apt-get" && c.Env["DEBIAN_FRONTEND"] != "noninteractive" {
			t.Errorf("apt-get run without DEBIAN_FRONTEND: %v", c)
		}
	}
}

func TestAptUpgradeNothingToDo(t *testing.T) {
	res := aptResult("0 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n")
	if res.Changed {
		t.Fatalf("expected unchanged, got %+v", res)
	}
}

func TestAptCheckFailure(t *testing.T) {
	f := (&runner.Fake{}).On("update", runner.Result{ExitCode: 100, Stderr: "E: Could not get lock"})
	_, err := (&Apt{r: f}).Check(context.Background())
	var exitErr *runner.ExitError
	if !errors.As(err, &exitErr) || !strings.Contains(err.Error(), "Could not get lock") {
		t.Fatalf("expected lock error, got %v", err)
	}
}

func TestParseDnfRepoquery(t *testing.T) {
	// dnf4 prints the \n escape literally, dnf5 expands it.
	out := "openssl-libs|1:3.2.4-1.fc43|updates\\n\nkernel-core|6.17.2-300.fc43|updates\n\nkernel-core|6.17.1-300.fc43|updates\n"
	updates := ParseDnfRepoquery(out)
	if len(updates) != 2 || updates[0].Name != "openssl-libs" || updates[0].Source != "updates" || updates[1].Version != "6.17.2-300.fc43" {
		t.Fatalf("unexpected %+v", updates)
	}
}

func TestSecurityPackages(t *testing.T) {
	dnf4 := `FEDORA-2025-1a2b3c4d5e Moderate/Sec.  openssl-libs-1:3.2.4-1.fc43.x86_64
FEDORA-2025-ffffffffff bugfix          tzdata-2025b-1.fc43.noarch
`
	dnf5 := `Name                Type     Severity                         Package              Issued
FEDORA-2025-1a2b3c4d5e security Moderate openssl-libs-1:3.2.4-1.fc43.x86_64 2025-10-01 12:00:00
FEDORA-2025-0000000000 security Important glibc-2.42-4.fc43.x86_64 2025-10-02 12:00:00
`
	got4 := SecurityPackages(dnf4)
	if !got4["openssl-libs"] || got4["tzdata"] || len(got4) != 1 {
		t.Errorf("dnf4: %v", got4)
	}
	got5 := SecurityPackages(dnf5)
	if !got5["openssl-libs"] || !got5["glibc"] || len(got5) != 2 {
		t.Errorf("dnf5: %v", got5)
	}
}

func TestDnfCheck(t *testing.T) {
	f := (&runner.Fake{}).
		On("makecache", runner.Result{}).
		On("repoquery", runner.Result{Stdout: "openssl-libs|1:3.2.4-1.fc43|updates\nvim-minimal|2:9.1-1.fc43|updates\n"}).
		On("updateinfo", runner.Result{Stdout: "FEDORA-2025-1 Moderate/Sec. openssl-libs-1:3.2.4-1.fc43.x86_64\n"}).
		On("needs-restarting", runner.Result{ExitCode: 1})
	st, err := (&Dnf{r: f}).Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Updates) != 2 || st.Security != 1 || !st.Updates[0].Security || !st.RebootRequired {
		t.Fatalf("unexpected %+v", st)
	}
}

func TestDnfCheckWithoutAdvisories(t *testing.T) {
	f := (&runner.Fake{}).
		On("makecache", runner.Result{}).
		On("repoquery", runner.Result{}).
		On("updateinfo", runner.Result{ExitCode: 2}).
		On("needs-restarting", runner.Result{ExitCode: 0})
	st, err := (&Dnf{r: f}).Check(context.Background())
	if err != nil || st.Security != -1 || st.RebootRequired || len(st.Updates) != 0 {
		t.Fatalf("unexpected %+v %v", st, err)
	}
}

func TestDnfUpgrade(t *testing.T) {
	f := (&runner.Fake{}).On("upgrade --refresh", runner.Result{Stdout: "Dependencies resolved.\nNothing to do.\nComplete!\n"})
	res, err := (&Dnf{r: f}).Upgrade(context.Background())
	if err != nil || res.Changed {
		t.Fatalf("unexpected %+v %v", res, err)
	}
}

const nixDry = `building the system configuration...
these 2 derivations will be built:
  /nix/store/0123456789abcdfghijklmnpqrsvwxyz-etc.drv
  /nix/store/abcdfghijklmnpqrsvwxyz0123456789-nixos-system-web-25.11.20251001.abcdef.drv
these 3 paths will be fetched (12.30 MiB download, 50.10 MiB unpacked):
  /nix/store/zyxwvsrqpnmlkjihgfdcba9876543210-openssl-3.4.1
  /nix/store/11111111111111111111111111111111-glibc-2.40-66
  /nix/store/22222222222222222222222222222222-systemd-257.9
`

func TestParseNixDryBuild(t *testing.T) {
	updates := ParseNixDryBuild(nixDry)
	if len(updates) != 5 {
		t.Fatalf("expected 5, got %+v", updates)
	}
	if updates[0].Name != "etc" || updates[0].Source != "build" || updates[2].Name != "openssl-3.4.1" || updates[2].Source != "fetch" {
		t.Fatalf("unexpected %+v", updates)
	}
	if got := ParseNixDryBuild("building the system configuration...\n"); len(got) != 0 {
		t.Fatalf("expected no updates, got %+v", got)
	}
}

func TestNixOSChannelAndFlake(t *testing.T) {
	ctx := context.Background()
	f := (&runner.Fake{}).
		On("nixos-rebuild dry-build --upgrade", runner.Result{Stderr: nixDry}).
		On("nixos-rebuild dry-build --flake github:me/cfg#web --refresh", runner.Result{}).
		On("/proc/sys/kernel/hostname", runner.Result{Stdout: "web\n"}).
		On("sh -c", runner.Result{ExitCode: 1})
	st, err := (&NixOS{r: f}).Check(ctx)
	if err != nil || len(st.Updates) != 5 || st.Security != -1 || st.RebootRequired {
		t.Fatalf("channel check: %+v %v", st, err)
	}
	st, err = (&NixOS{r: f, flake: "github:me/cfg"}).Check(ctx)
	if err != nil || len(st.Updates) != 0 {
		t.Fatalf("flake check: %+v %v", st, err)
	}
}

func TestNixOSUpgradeDetectsChange(t *testing.T) {
	gen := 1
	f := (&runner.Fake{}).
		OnFunc("readlink -f /run/current-system", func(runner.Cmd) runner.Result {
			return runner.Result{Stdout: strings.Repeat("x", gen) + "\n"}
		}).
		OnFunc("nixos-rebuild switch --upgrade", func(runner.Cmd) runner.Result { gen++; return runner.Result{} }).
		On("nix-collect-garbage --delete-older-than 14d", runner.Result{})
	n := &NixOS{r: f, gcOlderThan: "14d"}
	res, err := n.Upgrade(context.Background())
	if err != nil || !res.Changed {
		t.Fatalf("unexpected %+v %v", res, err)
	}
	if err := n.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestForUnsupported(t *testing.T) {
	_, err := For(osinfo.Info{Family: osinfo.FamilyUnknown, PrettyName: "Alpine Linux"}, &runner.Fake{}, Options{})
	var unsupported ErrUnsupported
	if !errors.As(err, &unsupported) || unsupported.OS != "Alpine Linux" {
		t.Fatalf("got %v", err)
	}
}

func TestSummary(t *testing.T) {
	st := Status{Updates: []Update{{Name: "a"}, {Name: "b"}, {Name: "c"}}}
	if got := Summary(st, 2); got != "a, b, and 1 more" {
		t.Errorf("got %q", got)
	}
	if got := Summary(Status{}, 2); got != "Up to date" {
		t.Errorf("got %q", got)
	}
}
