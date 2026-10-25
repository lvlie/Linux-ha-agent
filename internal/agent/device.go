package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/lvlie/linux-ha-agent/internal/commands"
	"github.com/lvlie/linux-ha-agent/internal/containers"
	"github.com/lvlie/linux-ha-agent/internal/guest"
	"github.com/lvlie/linux-ha-agent/internal/ha"
	"github.com/lvlie/linux-ha-agent/internal/packages"
	"github.com/lvlie/linux-ha-agent/internal/runner"
	"github.com/lvlie/linux-ha-agent/internal/sysinfo"
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

	Check *Check
	// Stats are the latest system statistics (CPU, memory, kernel, ...).
	Stats     *sysinfo.Info
	StatsTime time.Time
	LastRun   *RunReport
	Operation string // non-empty while an operation runs
	// discovery is the last published discovery payload, to republish only on change.
	discovery string
	// components are the entities in the last published discovery payload.
	components map[string]string
	// lastChecked is the discovery input from the last check, persisted
	// across restarts; used until the device is checked again.
	lastChecked *ha.Device
	// commands are the latest results of the configured commands by key.
	commands map[string]commands.Result
	// commandsRun is when each configured command last ran.
	commandsRun map[string]time.Time
	// lastCommand is the result of the last command typed in Home Assistant.
	lastCommand *commands.Result
	// commandRunning is set while a typed command runs.
	commandRunning bool
}

// commandState renders a command result for the state.
func commandState(r commands.Result) map[string]any {
	return map[string]any{
		"command": r.Command, "value": r.Value(), "output": r.Output,
		"exit_code": r.ExitCode, "time": r.Time.Format(time.RFC3339), "error": r.Error,
	}
}

// sharedKernel reports whether the device runs its host's kernel.
func (d *Device) sharedKernel() bool {
	return d.Guest != nil && d.Guest.Type == guest.Container
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
	case g.App != nil && g.App.ComposeProject != "":
		return "incus-compose service"
	case g.App != nil:
		return "Incus OCI container"
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

func (d *Device) haDevice(hostID string, containerEntities bool) ha.Device {
	if d.Check == nil && d.lastChecked != nil {
		dev := *d.lastChecked
		dev.Name, dev.Model = d.Name, d.model()
		return dev
	}
	dev := ha.Device{
		ID: d.ID, Name: d.Name, Model: d.model(), Host: d.Host, Engines: d.hasContainers(),
		SharedKernel: d.sharedKernel(),
		Temperature:  d.Stats != nil && d.Stats.TemperatureC != nil,
	}
	if d.Guest != nil {
		dev.Guest = true
		dev.ViaDevice = hostID
		dev.App = d.app() != nil
	}
	if containerEntities && d.Check != nil {
		for _, c := range d.Check.Containers {
			dev.Containers = append(dev.Containers, ha.Container{
				Key: ha.ContainerKey(c.Engine, c.Name), Name: c.Name, Managed: c.Project != "" || c.AutoUpdate,
			})
		}
	}
	return dev
}

// container finds a container of the latest check by its key.
func (d *Device) container(key string) (containers.Container, bool) {
	if d.Check == nil {
		return containers.Container{}, false
	}
	for _, c := range d.Check.Containers {
		if ha.ContainerKey(c.Engine, c.Name) == key {
			return c, true
		}
	}
	return containers.Container{}, false
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// containerStates renders the per-container part of the state.
func (d *Device) containerStates() map[string]any {
	out := map[string]any{}
	if d.Check == nil {
		return out
	}
	newer := map[string]string{} // container name -> newer digest
	for _, u := range d.Check.Images.Updates {
		for _, name := range u.Containers {
			newer[name] = u.Digest
		}
	}
	for _, c := range d.Check.Containers {
		installed := "unknown"
		if digests := d.Check.LocalDigests[c.ImageID]; len(digests) > 0 {
			installed = shortDigest(digests[0])
		} else if c.ImageID != "" {
			installed = shortDigest(c.ImageID)
		}
		latest := installed
		if digest, ok := newer[c.Name]; ok {
			latest = shortDigest(digest)
		}
		out[ha.ContainerKey(c.Engine, c.Name)] = map[string]any{
			"running": c.Running(),
			"info": map[string]any{
				"engine": c.Engine, "image": c.Image, "state": c.State, "project": c.Project,
				"service": c.Service, "update_available": latest != installed,
			},
			"update": updateInfo{
				InstalledVersion: strPtr(installed),
				LatestVersion:    strPtr(latest),
				Title:            c.Image,
				InProgress:       d.Operation != "",
			},
		}
	}
	return out
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
		"updates_available":  false,
		"kernel":             nil,
		"latest_kernel":      nil,
		"kernel_update":      false,
		"arch":               nil,
		"boot_time":          nil,
		"cpu_percent":        nil,
		"memory_percent":     nil,
		"memory_total":       nil,
		"memory_available":   nil,
		"disk_percent":       nil,
		"disk_total":         nil,
		"disk_used":          nil,
		"load_1":             nil,
		"load_5":             nil,
		"load_15":            nil,
		"temperature":        nil,
		"platform":           nil,
		"ctr":                d.containerStates(),
		"cmd":                map[string]any{},
		"command":            nil,
	}
	cmds := st["cmd"].(map[string]any)
	for key, r := range d.commands {
		cmds[key] = commandState(r)
	}
	if d.lastCommand != nil {
		st["command"] = commandState(*d.lastCommand)
	}
	if d.Guest != nil {
		st["platform"] = guestKind(*d.Guest)
	}
	if app := d.app(); app != nil {
		st["app"] = map[string]any{"image": app.Image, "compose_project": app.ComposeProject, "compose_service": app.ComposeService}
		current := shortDigest(app.Fingerprint)
		u := updateInfo{Title: app.Image, InstalledVersion: strPtr(current), LatestVersion: strPtr(current), InProgress: d.Operation != ""}
		if d.Check != nil && d.Check.App != nil {
			st["app"].(map[string]any)["error"] = d.Check.App.Error
			if d.Check.App.UpdateAvailable() {
				u.LatestVersion = strPtr(shortDigest(d.Check.App.Latest))
				st["image_update_list"] = []containers.ImageUpdate{{Image: app.Image, Digest: d.Check.App.Latest, Containers: []string{d.Name}, Managed: true}}
			}
			st["image_updates"] = d.pendingImages()
		}
		st["app_update"] = u
	}
	if s := d.Stats; s != nil {
		st["kernel"] = s.Kernel
		st["arch"] = s.Arch
		if s.LatestKernel != "" {
			st["latest_kernel"] = s.LatestKernel
		}
		st["kernel_update"] = s.KernelUpdatePending()
		st["boot_time"] = s.BootTime(d.StatsTime).Format(time.RFC3339)
		st["cpu_percent"] = s.CPUPercent
		st["memory_percent"] = s.MemUsedPercent()
		st["memory_total"] = s.MemTotal
		st["memory_available"] = s.MemAvailable
		st["disk_percent"] = s.DiskUsedPercent()
		st["disk_total"] = s.DiskTotal
		st["disk_used"] = s.DiskUsed
		st["load_1"] = s.Load1
		st["load_5"] = s.Load5
		st["load_15"] = s.Load15
		st["temperature"] = s.TemperatureC
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
		st["reboot_required"] = d.rebootRequired()
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
	st["updates_available"] = d.pendingUpdates() > 0 || d.pendingImages() > 0
	st["reboot_required"] = d.rebootRequired()
	for k, v := range extra {
		st[k] = v
	}
	return st
}

func (d *Device) pendingUpdates() int {
	if d.Check == nil || d.Check.Packages == nil {
		return 0
	}
	return len(d.Check.Packages.Updates)
}

func (d *Device) pendingImages() int {
	if d.Check == nil {
		return 0
	}
	if d.Check.App != nil {
		if d.Check.App.UpdateAvailable() {
			return 1
		}
		return 0
	}
	return len(d.Check.Images.Updates)
}

func (d *Device) rebootRequired() bool {
	return (d.Check != nil && d.Check.RebootRequired) || (d.Stats != nil && !d.sharedKernel() && d.Stats.KernelUpdatePending())
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
