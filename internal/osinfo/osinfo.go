// Package osinfo detects the distribution of a target from /etc/os-release.
package osinfo

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// Family groups distributions that are managed the same way.
type Family string

// Supported families.
const (
	FamilyDebian  Family = "debian"
	FamilyRHEL    Family = "rhel"
	FamilyNixOS   Family = "nixos"
	FamilyUnknown Family = "unknown"
)

// Info describes the operating system of a target.
type Info struct {
	ID         string
	IDLike     []string
	Name       string
	Version    string
	PrettyName string
	Family     Family
}

// Detect reads /etc/os-release on the target.
func Detect(ctx context.Context, r runner.Runner) (Info, error) {
	res, err := runner.Check(ctx, r, runner.Command("cat", "/etc/os-release"))
	if err != nil {
		return Info{Family: FamilyUnknown}, fmt.Errorf("reading /etc/os-release: %w", err)
	}
	return Parse(res.Stdout), nil
}

// Parse parses the contents of an os-release file.
func Parse(content string) Info {
	values := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		} else {
			value = strings.Trim(value, `"'`)
		}
		values[key] = value
	}
	info := Info{
		ID:         strings.ToLower(values["ID"]),
		IDLike:     strings.Fields(strings.ToLower(values["ID_LIKE"])),
		Name:       values["NAME"],
		Version:    values["VERSION_ID"],
		PrettyName: values["PRETTY_NAME"],
	}
	if info.PrettyName == "" {
		info.PrettyName = strings.TrimSpace(info.Name + " " + info.Version)
	}
	info.Family = family(info)
	return info
}

func family(info Info) Family {
	ids := append([]string{info.ID}, info.IDLike...)
	switch {
	case slices.Contains(ids, "nixos"):
		return FamilyNixOS
	case slices.Contains(ids, "debian") || slices.Contains(ids, "ubuntu"):
		return FamilyDebian
	case slices.ContainsFunc(ids, func(id string) bool {
		return id == "fedora" || id == "rhel" || id == "centos"
	}):
		return FamilyRHEL
	default:
		return FamilyUnknown
	}
}
