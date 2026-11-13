// Package packages checks for and installs operating system updates with the
// native package manager of a target: apt, dnf, NixOS or Synology DSM.
package packages

import (
	"context"
	"fmt"

	"github.com/lvlie/linux-ha-agent/internal/osinfo"
	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// Update is one pending package update.
type Update struct {
	Name string `json:"name"`
	// From is the installed version, where the package manager says it.
	From     string `json:"from,omitempty"`
	Version  string `json:"version,omitempty"`
	Source   string `json:"source,omitempty"`
	Security bool   `json:"security,omitempty"`
}

// Status is the result of a check.
type Status struct {
	Manager string   `json:"manager"`
	Updates []Update `json:"updates"`
	// Security is the number of security updates, or -1 when unknown.
	Security       int  `json:"security"`
	RebootRequired bool `json:"reboot_required"`
}

// Result of an upgrade.
type Result struct {
	Changed bool   `json:"changed"`
	Summary string `json:"summary"`
}

// Manager is a package manager on one target.
type Manager interface {
	Name() string
	// Check refreshes package metadata and reports pending updates.
	Check(ctx context.Context) (Status, error)
	// Upgrade installs all pending updates.
	Upgrade(ctx context.Context) (Result, error)
	// Cleanup removes unused packages and cached downloads.
	Cleanup(ctx context.Context) error
	// RebootRequired reports whether the target needs a reboot.
	RebootRequired(ctx context.Context) (bool, error)
}

// Options tune the managers.
type Options struct {
	// NixFlake rebuilds NixOS from a flake instead of channels.
	NixFlake string
	// NixGCOlderThan is passed to nix-collect-garbage --delete-older-than.
	NixGCOlderThan string
}

// ErrUnsupported is returned for distributions without a manager.
type ErrUnsupported struct{ OS string }

func (e ErrUnsupported) Error() string {
	return fmt.Sprintf("no supported package manager for %s", e.OS)
}

// For picks the manager for the detected OS.
func For(info osinfo.Info, r runner.Runner, opts Options) (Manager, error) {
	switch info.Family {
	case osinfo.FamilyDebian:
		return &Apt{r: r}, nil
	case osinfo.FamilyRHEL:
		return &Dnf{r: r}, nil
	case osinfo.FamilyNixOS:
		return &NixOS{r: r, flake: opts.NixFlake, gcOlderThan: opts.NixGCOlderThan}, nil
	case osinfo.FamilyDSM:
		return &DSM{r: r}, nil
	default:
		name := info.PrettyName
		if name == "" {
			name = info.ID
		}
		return nil, ErrUnsupported{OS: name}
	}
}

// Summary lists the updates as "name → version" within maxLen characters,
// ending in "and N more" for those that do not fit.
func Summary(s Status, maxLen int) string {
	if len(s.Updates) == 0 {
		return "Up to date"
	}
	more := func(n int) string {
		if n == 0 {
			return ""
		}
		return fmt.Sprintf(", and %d more", n)
	}
	var out string
	for i, u := range s.Updates {
		entry := u.Name
		if u.Version != "" {
			entry += " → " + u.Version
		}
		next := entry
		if out != "" {
			next = out + ", " + entry
		}
		if len(next)+len(more(len(s.Updates)-i-1)) > maxLen {
			if out == "" {
				return fmt.Sprintf("%d updates", len(s.Updates))
			}
			return out + more(len(s.Updates)-i)
		}
		out = next
	}
	return out
}

// String describes an update with the versions known:
// "libc6 2.39-0ubuntu8.3 → 2.39-0ubuntu8.4".
func (u Update) String() string {
	switch {
	case u.From != "" && u.Version != "":
		return u.Name + " " + u.From + " → " + u.Version
	case u.Version != "":
		return u.Name + " → " + u.Version
	}
	return u.Name
}
