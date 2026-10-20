// Package incus talks to the local Incus daemon through its REST API: it lists
// guests, runs commands inside them (like `incus exec`), and manages
// snapshots, restarts and lifecycle events.
package incus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"

	"github.com/lvlie/linux-ha-agent/internal/guest"
	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// API implements guest.Provider on top of the Incus Go client.
type API struct {
	server incusclient.InstanceServer
}

// Connect connects to the Incus unix socket. An empty path uses the default.
func Connect(socket string) (*API, error) {
	server, err := incusclient.ConnectIncusUnix(socket, nil)
	if err != nil {
		return nil, fmt.Errorf("connecting to incus at %q: %w", socket, err)
	}
	return &API{server: server}, nil
}

// Platform implements guest.Provider.
func (*API) Platform() string { return "incus" }

// Guests lists all instances in all projects.
func (a *API) Guests(_ context.Context) ([]guest.Guest, error) {
	instances, err := a.server.GetInstancesAllProjects(api.InstanceTypeAny)
	if err != nil {
		return nil, fmt.Errorf("listing instances: %w", err)
	}
	guests := make([]guest.Guest, 0, len(instances))
	for _, inst := range instances {
		project := inst.Project
		if project == "" {
			project = api.ProjectDefaultName
		}
		guests = append(guests, guest.Guest{Platform: "incus", Name: inst.Name, Project: project, Type: inst.Type, Status: inst.Status})
	}
	slices.SortFunc(guests, func(a, b guest.Guest) int { return strings.Compare(a.Key(), b.Key()) })
	return guests, nil
}

func (a *API) project(g guest.Guest) incusclient.InstanceServer {
	return a.server.UseProject(g.Project)
}

// Runner returns a Runner that executes commands inside the guest.
func (a *API) Runner(g guest.Guest) runner.Runner {
	return &guestRunner{api: a, guest: g}
}

type guestRunner struct {
	api   *API
	guest guest.Guest
}

// Run executes the command like `incus exec`, streaming output over the
// operation's websockets.
func (r *guestRunner) Run(ctx context.Context, cmd runner.Cmd) (runner.Result, error) {
	if len(cmd.Args) == 0 {
		return runner.Result{}, errors.New("empty command")
	}
	var stdout, stderr bytes.Buffer
	done := make(chan bool)
	op, err := r.api.project(r.guest).ExecInstance(r.guest.Name, api.InstanceExecPost{
		Command:     cmd.Args,
		Environment: runner.Environment(cmd),
		WaitForWS:   true,
		Interactive: false,
	}, &incusclient.InstanceExecArgs{
		Stdin:    bytes.NewReader(nil),
		Stdout:   &stdout,
		Stderr:   &stderr,
		DataDone: done,
	})
	if err != nil {
		return runner.Result{}, fmt.Errorf("exec in %s: %w", r.guest.Key(), err)
	}
	if err := op.WaitContext(ctx); err != nil {
		if ctx.Err() != nil {
			_ = op.Cancel()
		}
		return runner.Result{Stdout: stdout.String(), Stderr: stderr.String()}, fmt.Errorf("exec in %s: %w", r.guest.Key(), err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		return runner.Result{}, ctx.Err()
	}
	res := runner.Result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: -1}
	if code, ok := op.Get().Metadata["return"].(float64); ok {
		res.ExitCode = int(code)
	}
	return res, nil
}

// CreateSnapshot takes a snapshot that Incus removes after expires.
func (a *API) CreateSnapshot(ctx context.Context, g guest.Guest, name string, expires time.Time) error {
	req := api.InstanceSnapshotsPost{Name: name}
	if !expires.IsZero() {
		req.ExpiresAt = &expires
	}
	op, err := a.project(g).CreateInstanceSnapshot(g.Name, req)
	if err != nil {
		return fmt.Errorf("snapshot of %s: %w", g.Key(), err)
	}
	if err := op.WaitContext(ctx); err != nil {
		return fmt.Errorf("snapshot of %s: %w", g.Key(), err)
	}
	return nil
}

// Snapshots lists the guest's snapshots, newest first.
func (a *API) Snapshots(_ context.Context, g guest.Guest) ([]guest.Snapshot, error) {
	snaps, err := a.project(g).GetInstanceSnapshots(g.Name)
	if err != nil {
		return nil, fmt.Errorf("listing snapshots of %s: %w", g.Key(), err)
	}
	out := make([]guest.Snapshot, 0, len(snaps))
	for _, s := range snaps {
		name := s.Name
		if _, after, ok := strings.Cut(name, "/"); ok {
			name = after
		}
		out = append(out, guest.Snapshot{Name: name, CreatedAt: s.CreatedAt})
	}
	slices.SortFunc(out, func(a, b guest.Snapshot) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, nil
}

// RestoreSnapshot restores the guest to the named snapshot.
func (a *API) RestoreSnapshot(ctx context.Context, g guest.Guest, name string) error {
	server := a.project(g)
	inst, etag, err := server.GetInstance(g.Name)
	if err != nil {
		return fmt.Errorf("restore of %s: %w", g.Key(), err)
	}
	put := inst.Writable()
	put.Restore = name
	op, err := server.UpdateInstance(g.Name, put, etag)
	if err != nil {
		return fmt.Errorf("restore of %s to %s: %w", g.Key(), name, err)
	}
	if err := op.WaitContext(ctx); err != nil {
		return fmt.Errorf("restore of %s to %s: %w", g.Key(), name, err)
	}
	return nil
}

// Restart restarts the guest, waiting up to timeout for a clean shutdown.
func (a *API) Restart(ctx context.Context, g guest.Guest, timeout time.Duration) error {
	op, err := a.project(g).UpdateInstanceState(g.Name, api.InstanceStatePut{
		Action:  "restart",
		Timeout: int(timeout.Seconds()),
	}, "")
	if err != nil {
		return fmt.Errorf("restart of %s: %w", g.Key(), err)
	}
	if err := op.WaitContext(ctx); err != nil {
		return fmt.Errorf("restart of %s: %w", g.Key(), err)
	}
	return nil
}

var lifecycleActions = []string{
	"instance-created", "instance-deleted", "instance-started", "instance-stopped",
	"instance-restarted", "instance-renamed", "instance-shutdown", "instance-restored",
}

// WatchLifecycle implements guest.Provider.
func (a *API) WatchLifecycle(ctx context.Context, fn func()) error {
	listener, err := a.server.GetEventsAllProjectsByType([]string{api.EventTypeLifecycle})
	if err != nil {
		return fmt.Errorf("subscribing to incus events: %w", err)
	}
	defer listener.Disconnect()
	_, err = listener.AddHandler([]string{api.EventTypeLifecycle}, func(event api.Event) {
		var lifecycle api.EventLifecycle
		if err := json.Unmarshal(event.Metadata, &lifecycle); err == nil && slices.Contains(lifecycleActions, lifecycle.Action) {
			fn()
		}
	})
	if err != nil {
		return fmt.Errorf("subscribing to incus events: %w", err)
	}
	errc := make(chan error, 1)
	go func() { errc <- listener.Wait() }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return fmt.Errorf("incus event stream: %w", err)
	}
}
