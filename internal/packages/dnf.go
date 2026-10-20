package packages

import (
	"context"
	"strings"

	"github.com/lvlie/incus-ha-agent/internal/runner"
)

// Dnf manages Fedora, RHEL and derivatives. It works with dnf4 and dnf5.
type Dnf struct {
	r runner.Runner
}

// Name implements Manager.
func (*Dnf) Name() string { return "dnf" }

// Check implements Manager.
func (d *Dnf) Check(ctx context.Context) (Status, error) {
	if _, err := runner.Check(ctx, d.r, runner.Command("dnf", "-q", "-y", "makecache", "--refresh")); err != nil {
		return Status{}, err
	}
	// dnf5 needs the explicit newline; dnf4 adds one itself (empty lines are skipped).
	res, err := runner.Check(ctx, d.r, runner.Command("dnf", "-q", "repoquery", "--upgrades", "--latest-limit=1",
		"--queryformat", `%{name}|%{evr}|%{repoid}\n`))
	if err != nil {
		return Status{}, err
	}
	st := Status{Manager: d.Name(), Updates: ParseDnfRepoquery(res.Stdout), Security: -1}

	// Advisories are optional metadata; an error just means "unknown".
	adv, err := d.r.Run(ctx, runner.Command("dnf", "-q", "updateinfo", "list", "--security"))
	if err == nil && adv.ExitCode == 0 {
		names := SecurityPackages(adv.Stdout)
		st.Security = 0
		for i := range st.Updates {
			if names[st.Updates[i].Name] {
				st.Updates[i].Security = true
				st.Security++
			}
		}
	}
	st.RebootRequired, err = d.RebootRequired(ctx)
	return st, err
}

// ParseDnfRepoquery parses "name|evr|repo" lines.
func ParseDnfRepoquery(out string) []Update {
	seen := map[string]bool{}
	var updates []Update
	for _, line := range strings.Split(out, "\n") {
		// dnf4 does not expand the \n escape and prints it literally.
		line = strings.TrimSuffix(strings.TrimSpace(line), `\n`)
		parts := strings.Split(line, "|")
		if len(parts) != 3 || parts[0] == "" || seen[parts[0]] {
			continue
		}
		seen[parts[0]] = true
		updates = append(updates, Update{Name: parts[0], Version: parts[1], Source: parts[2]})
	}
	return updates
}

// SecurityPackages extracts package names from `dnf updateinfo list
// --security`. dnf4 prints "ADVISORY  Severity/Sec.  nevra", dnf5 prints
// "ADVISORY security Severity nevra date".
func SecurityPackages(out string) map[string]bool {
	names := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || strings.EqualFold(fields[0], "name") {
			continue
		}
		isSecurity := false
		nevra := ""
		for i, f := range fields[1:] {
			lower := strings.ToLower(f)
			if strings.Contains(lower, "sec") {
				isSecurity = true
			}
			if nevra == "" && i > 0 && strings.Count(f, "-") >= 2 {
				nevra = f
			}
		}
		if !isSecurity {
			continue
		}
		if nevra == "" {
			nevra = fields[len(fields)-1]
		}
		names[nevraName(nevra)] = true
	}
	return names
}

// nevraName strips version, release and arch: "openssl-libs-1:3.2.4-1.fc43.x86_64" -> "openssl-libs".
func nevraName(nevra string) string {
	parts := strings.Split(nevra, "-")
	if len(parts) < 3 {
		return nevra
	}
	return strings.Join(parts[:len(parts)-2], "-")
}

// Upgrade implements Manager.
func (d *Dnf) Upgrade(ctx context.Context) (Result, error) {
	res, err := runner.Check(ctx, d.r, runner.Command("dnf", "-y", "upgrade", "--refresh"))
	if err != nil {
		return Result{}, err
	}
	if strings.Contains(res.Stdout+res.Stderr, "Nothing to do") {
		return Result{Summary: "nothing to do"}, nil
	}
	return Result{Changed: true, Summary: "packages upgraded"}, nil
}

// Cleanup implements Manager.
func (d *Dnf) Cleanup(ctx context.Context) error {
	if _, err := runner.Check(ctx, d.r, runner.Command("dnf", "-y", "autoremove")); err != nil {
		return err
	}
	_, err := runner.Check(ctx, d.r, runner.Command("dnf", "clean", "packages"))
	return err
}

// RebootRequired implements Manager. `dnf needs-restarting -r` exits 1 when
// a reboot is needed; anything else unexpected is treated as "no".
func (d *Dnf) RebootRequired(ctx context.Context) (bool, error) {
	res, err := d.r.Run(ctx, runner.Command("dnf", "-q", "needs-restarting", "-r"))
	if err != nil {
		return false, err
	}
	return res.ExitCode == 1, nil
}
