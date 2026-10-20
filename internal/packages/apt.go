package packages

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// Apt manages Debian and Ubuntu.
type Apt struct {
	r runner.Runner
}

// Name implements Manager.
func (*Apt) Name() string { return "apt" }

var aptEnv = []string{
	"DEBIAN_FRONTEND", "noninteractive",
	// Restart services after upgrades without asking (needrestart on Ubuntu).
	"NEEDRESTART_MODE", "a",
}

// Wait for other apt runs (e.g. unattended-upgrades) instead of failing.
const aptLockTimeout = "DPkg::Lock::Timeout=600"

func aptGet(args ...string) runner.Cmd {
	base := []string{"apt-get", "-q", "-o", aptLockTimeout}
	return runner.Command(append(base, args...)...).WithEnv(aptEnv...)
}

// Check implements Manager.
func (a *Apt) Check(ctx context.Context) (Status, error) {
	if _, err := runner.Check(ctx, a.r, aptGet("update")); err != nil {
		return Status{}, err
	}
	res, err := runner.Check(ctx, a.r, aptGet("-s", "dist-upgrade"))
	if err != nil {
		return Status{}, err
	}
	st := Status{Manager: a.Name(), Updates: ParseAptSimulation(res.Stdout)}
	for _, u := range st.Updates {
		if u.Security {
			st.Security++
		}
	}
	st.RebootRequired, err = a.RebootRequired(ctx)
	return st, err
}

// "Inst libc6 [2.39-0ubuntu8.3] (2.39-0ubuntu8.4 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])"
var aptInst = regexp.MustCompile(`^Inst (\S+)(?: \[[^\]]*\])? \((\S+) ([^\[)]*)`)

// ParseAptSimulation parses the output of `apt-get -s dist-upgrade`.
func ParseAptSimulation(out string) []Update {
	var updates []Update
	for _, line := range strings.Split(out, "\n") {
		m := aptInst.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		source := strings.TrimSpace(m[3])
		updates = append(updates, Update{
			Name:     m[1],
			Version:  m[2],
			Source:   source,
			Security: strings.Contains(strings.ToLower(source), "security"),
		})
	}
	return updates
}

var aptCounts = regexp.MustCompile(`(?m)^(\d+) upgraded, (\d+) newly installed, (\d+) to remove`)

// Upgrade implements Manager.
func (a *Apt) Upgrade(ctx context.Context) (Result, error) {
	res, err := runner.Check(ctx, a.r, aptGet("-y",
		"-o", "Dpkg::Options::=--force-confdef",
		"-o", "Dpkg::Options::=--force-confold",
		"dist-upgrade"))
	if err != nil {
		return Result{}, err
	}
	return aptResult(res.Stdout), nil
}

func aptResult(out string) Result {
	m := aptCounts.FindStringSubmatch(out)
	if m == nil {
		return Result{Changed: true, Summary: "upgrade finished"}
	}
	upgraded, _ := strconv.Atoi(m[1])
	installed, _ := strconv.Atoi(m[2])
	removed, _ := strconv.Atoi(m[3])
	return Result{
		Changed: upgraded+installed+removed > 0,
		Summary: fmt.Sprintf("%d upgraded, %d newly installed, %d removed", upgraded, installed, removed),
	}
}

// Cleanup implements Manager.
func (a *Apt) Cleanup(ctx context.Context) error {
	if _, err := runner.Check(ctx, a.r, aptGet("-y", "autoremove", "--purge")); err != nil {
		return err
	}
	_, err := runner.Check(ctx, a.r, aptGet("clean"))
	return err
}

// aptRebootCheck exits 0 when a reboot is needed: Ubuntu writes
// /var/run/reboot-required, but Debian and Proxmox VE do not, so on machines
// with their own kernel the newest installed kernel is compared with the
// running one. Containers run the host's kernel and skip that comparison.
const aptRebootCheck = `[ -e /var/run/reboot-required ] && exit 0
if [ -e /run/systemd/container ] || grep -qa container= /proc/1/environ 2>/dev/null; then exit 1; fi
latest=$(ls /boot/vmlinuz-* 2>/dev/null | sed 's|^/boot/vmlinuz-||' | sort -V | tail -n 1)
[ -n "$latest" ] && [ "$latest" != "$(uname -r)" ]`

// RebootRequired implements Manager.
func (a *Apt) RebootRequired(ctx context.Context) (bool, error) {
	res, err := a.r.Run(ctx, runner.Command("sh", "-c", aptRebootCheck))
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}
