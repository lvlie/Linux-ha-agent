package packages

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// NixOS rebuilds the system from channels or a flake.
type NixOS struct {
	r           runner.Runner
	flake       string
	gcOlderThan string
}

// Name implements Manager.
func (*NixOS) Name() string { return "nixos" }

func (n *NixOS) rebuildArgs(ctx context.Context, action string) ([]string, error) {
	args := []string{"nixos-rebuild", action}
	if n.flake == "" {
		return append(args, "--upgrade"), nil
	}
	ref := n.flake
	if !strings.Contains(ref, "#") {
		res, err := runner.Check(ctx, n.r, runner.Command("cat", "/proc/sys/kernel/hostname"))
		if err != nil {
			return nil, err
		}
		ref += "#" + strings.TrimSpace(res.Stdout)
	}
	return append(args, "--flake", ref, "--refresh"), nil
}

// Check implements Manager: a dry build lists what an upgrade would build or
// download. Nothing to build or fetch means the system is up to date.
func (n *NixOS) Check(ctx context.Context) (Status, error) {
	args, err := n.rebuildArgs(ctx, "dry-build")
	if err != nil {
		return Status{}, err
	}
	res, err := runner.Check(ctx, n.r, runner.Command(args...))
	if err != nil {
		return Status{}, err
	}
	st := Status{Manager: n.Name(), Updates: ParseNixDryBuild(res.Stdout + "\n" + res.Stderr), Security: -1}
	st.RebootRequired, err = n.RebootRequired(ctx)
	return st, err
}

var (
	nixHeader    = regexp.MustCompile(`these (\d+) (derivations will be built|paths will be fetched)`)
	nixStorePath = regexp.MustCompile(`^/nix/store/[a-z0-9]{32}-(.+?)(\.drv)?$`)
)

// ParseNixDryBuild extracts the derivations to build and paths to fetch.
func ParseNixDryBuild(out string) []Update {
	var updates []Update
	section := ""
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if m := nixHeader.FindStringSubmatch(line); m != nil {
			section = "build"
			if strings.HasPrefix(m[2], "paths") {
				section = "fetch"
			}
			continue
		}
		m := nixStorePath.FindStringSubmatch(line)
		if m == nil || section == "" || seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		updates = append(updates, Update{Name: m[1], Source: section})
	}
	return updates
}

// Upgrade implements Manager.
func (n *NixOS) Upgrade(ctx context.Context) (Result, error) {
	before := n.currentSystem(ctx)
	args, err := n.rebuildArgs(ctx, "switch")
	if err != nil {
		return Result{}, err
	}
	if _, err := runner.Check(ctx, n.r, runner.Command(args...)); err != nil {
		return Result{}, err
	}
	after := n.currentSystem(ctx)
	if before != "" && before == after {
		return Result{Summary: "system unchanged"}, nil
	}
	return Result{Changed: true, Summary: fmt.Sprintf("switched to %s", after)}, nil
}

func (n *NixOS) currentSystem(ctx context.Context) string {
	res, err := n.r.Run(ctx, runner.Command("readlink", "-f", "/run/current-system"))
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

// Cleanup implements Manager.
func (n *NixOS) Cleanup(ctx context.Context) error {
	args := []string{"nix-collect-garbage"}
	if n.gcOlderThan != "" {
		args = append(args, "--delete-older-than", n.gcOlderThan)
	}
	_, err := runner.Check(ctx, n.r, runner.Command(args...))
	return err
}

// A reboot is needed when the booted kernel, initrd or modules differ from
// the current system. Containers have none of these, so they never need one.
const nixRebootCheck = `for f in initrd kernel kernel-modules; do
  [ "$(readlink -f /run/booted-system/$f)" = "$(readlink -f /run/current-system/$f)" ] || exit 0
done
exit 1`

// RebootRequired implements Manager.
func (n *NixOS) RebootRequired(ctx context.Context) (bool, error) {
	res, err := n.r.Run(ctx, runner.Command("sh", "-c", nixRebootCheck))
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}
