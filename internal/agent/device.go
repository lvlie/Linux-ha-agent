package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/lvlie/linux-ha-agent/internal/containers"
	"github.com/lvlie/linux-ha-agent/internal/guest"
	"github.com/lvlie/linux-ha-agent/internal/ha"
	"github.com/lvlie/linux-ha-agent/internal/packages"
	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// Device is a machine managed by the agent: the host or an Incus guest.
type Device struct {
	ID    string
	Name  string
	Host  bool
	Guest *guest.Guest
	// Provider manages the guest; nil for the host.
	Provider guest.Provider
	Runner   runner.Runner

	Check     *Check
	LastRun   *RunReport
	Operation string // non-empty while an operation runs
	// discovery is the last published discovery payload, to republish only on change.
	discovery string
}

func (d *Device) running() bool {
	return d.Guest == nil || d.Guest.Running()
}

// manageable reports whether commands can run on the device.
func (d *Device) manageable() bool {
	return d.Guest == nil || d.Guest.Unmanageable == ""
}

func (d *Device) model() string {
	switch {
	case d.Check != nil && d.Check.OS != "":
		if d.Guest != nil {
			return fmt.Sprintf("%s (%s)", d.Check.OS, guestKind(*d.Guest))
		}
		return d.Check.OS
	case d.Guest != nil:
		return guestKind(*d.Guest)
	default:
		return "Linux host"
	}
}

// guestKind describes a guest the way its platform calls it.
func guestKind(g guest.Guest) string {
	switch {
	case g.Platform == "proxmox" && g.Type == guest.VirtualMachine:
		return "Proxmox VM"
	case g.Platform == "proxmox":
		return "Proxmox LXC"
	case g.Type == guest.VirtualMachine:
		return "Incus VM"
	default:
		return "Incus container"
	}
}

func (d *Device) hasContainers() bool {
	return d.Check != nil && len(d.Check.Engines) > 0
}

func (d *Device) haDevice(hostID string) ha.Device {
	dev := ha.Device{ID: d.ID, Name: d.Name, Model: d.model(), Host: d.Host, Containers: d.hasContainers()}
	if d.Guest != nil {
		dev.Guest = true
		dev.ViaDevice = hostID
	}
	return dev
}

// updateInfo is the payload of a Home Assistant update entity.
type updateInfo struct {
	InstalledVersion *string `json:"installed_version"`
	LatestVersion    *string `json:"latest_version"`
	Title            string  `json:"title"`
	ReleaseSummary   string  `json:"release_summary,omitempty"`
	InProgress       bool    `json:"in_progress"`
}

func strPtr(s string) *string { return &s }

// state renders the JSON published on the device's state topic.
func (d *Device) state(extra map[string]any) map[string]any {
	st := map[string]any{
		"running":            d.running(),
		"in_progress":        d.Operation != "",
		"operation":          d.Operation,
		"os":                 nil,
		"package_manager":    nil,
		"updates":            nil,
		"security_updates":   nil,
		"update_list":        []string{},
		"reboot_required":    false,
		"containers":         0,
		"containers_running": 0,
		"engines":            []string{},
		"projects":           []containers.Project{},
		"image_updates":      nil,
		"image_update_list":  []containers.ImageUpdate{},
		"images_unknown":     0,
		"last_check":         nil,
		"check_error":        "",
		"last_run":           d.LastRun,
		"last_run_result":    d.LastRun.Result(),
		"last_run_time":      nil,
	}
	if d.LastRun != nil {
		st["last_run_time"] = d.LastRun.Finished.Format(time.RFC3339)
	}

	system := updateInfo{Title: "System packages", InProgress: d.Operation != ""}
	images := updateInfo{Title: "Container images", InProgress: d.Operation != ""}

	if c := d.Check; c != nil {
		st["os"] = c.OS
		st["last_check"] = c.Time.Format(time.RFC3339)
		st["check_error"] = c.Error
		st["reboot_required"] = c.RebootRequired
		if p := c.Packages; p != nil {
			names := make([]string, 0, len(p.Updates))
			for _, u := range p.Updates {
				names = append(names, u.Name)
			}
			if len(names) > 50 {
				names = append(names[:50], fmt.Sprintf("... and %d more", len(names)-50))
			}
			st["package_manager"] = p.Manager
			st["updates"] = len(p.Updates)
			st["update_list"] = names
			if p.Security >= 0 {
				st["security_updates"] = p.Security
			}
			system.InstalledVersion = strPtr(c.OS)
			system.LatestVersion = strPtr(c.OS)
			if len(p.Updates) > 0 {
				system.LatestVersion = strPtr(fmt.Sprintf("%s + %d updates", c.OS, len(p.Updates)))
				system.ReleaseSummary = truncate(packages.Summary(*p, 15), 255)
			}
		}
		running := 0
		for _, ctr := range c.Containers {
			if ctr.Running() {
				running++
			}
		}
		st["containers"] = len(c.Containers)
		st["containers_running"] = running
		if c.Engines != nil {
			st["engines"] = c.Engines
		}
		if c.Projects != nil {
			st["projects"] = c.Projects
		}
		if c.ImagesChecked {
			st["image_updates"] = len(c.Images.Updates)
			if c.Images.Updates != nil {
				st["image_update_list"] = c.Images.Updates
			}
			st["images_unknown"] = c.Images.Unknown
			current := fmt.Sprintf("%d images", imageCount(c.Containers))
			images.InstalledVersion = strPtr(current)
			images.LatestVersion = strPtr(current)
			if n := len(c.Images.Updates); n > 0 {
				images.LatestVersion = strPtr(fmt.Sprintf("%s, %d newer", current, n))
				names := make([]string, 0, n)
				for _, u := range c.Images.Updates {
					names = append(names, u.Image)
				}
				images.ReleaseSummary = truncate(strings.Join(names, ", "), 255)
			}
		}
	}
	st["system_update"] = system
	st["container_update"] = images
	for k, v := range extra {
		st[k] = v
	}
	return st
}

func imageCount(cs []containers.Container) int {
	seen := map[string]bool{}
	for _, c := range cs {
		seen[c.Image] = true
	}
	return len(seen)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
