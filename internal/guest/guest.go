// Package guest defines what the agent needs from a virtualisation platform
// (Incus, Proxmox VE) to manage the containers and VMs running on a host.
package guest

import (
	"context"
	"strings"
	"time"

	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// Guest types.
const (
	Container      = "container"
	VirtualMachine = "virtual-machine"
)

// Guest is a container or VM on the host.
type Guest struct {
	Platform string // "incus" or "proxmox"
	Name     string
	// Project is the Incus project; empty on other platforms.
	Project string
	// ID is the platform's own identifier (the Proxmox VMID); empty for Incus.
	ID     string
	Type   string // Container or VirtualMachine
	Status string
	// Unmanageable explains why commands cannot run inside the guest (for
	// example a VM without guest agent). Empty when the guest is manageable.
	Unmanageable string
}

// Key identifies a guest on its platform.
func (g Guest) Key() string {
	switch {
	case g.Project != "":
		return g.Project + "/" + g.Name
	case g.ID != "":
		return g.ID
	default:
		return g.Name
	}
}

// Running reports whether the guest is running.
func (g Guest) Running() bool {
	return strings.EqualFold(g.Status, "running")
}

// Snapshot of a guest.
type Snapshot struct {
	Name      string
	CreatedAt time.Time
}

// Provider manages the guests of one platform on this host.
type Provider interface {
	Platform() string
	Guests(ctx context.Context) ([]Guest, error)
	// Runner runs commands inside the guest.
	Runner(g Guest) runner.Runner
	// CreateSnapshot takes a snapshot that is removed after expires.
	CreateSnapshot(ctx context.Context, g Guest, name string, expires time.Time) error
	// Snapshots lists snapshots, newest first.
	Snapshots(ctx context.Context, g Guest) ([]Snapshot, error)
	RestoreSnapshot(ctx context.Context, g Guest, name string) error
	Restart(ctx context.Context, g Guest, timeout time.Duration) error
	// WatchLifecycle calls fn whenever guests are created, deleted, started
	// or stopped. It blocks until ctx is done or watching fails.
	WatchLifecycle(ctx context.Context, fn func()) error
}
