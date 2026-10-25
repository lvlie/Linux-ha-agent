// Package proxmox manages the guests of a Proxmox VE node with the node's
// own tools: pvesh for the API, `pct exec` to run commands in LXC containers
// and the QEMU guest agent (`qm guest exec`) to run them in VMs.
package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lvlie/linux-ha-agent/internal/guest"
	"github.com/lvlie/linux-ha-agent/internal/runner"
)

const (
	kindLXC  = "lxc"
	kindQEMU = "qemu"
)

// Provider implements guest.Provider for one Proxmox VE node.
type Provider struct {
	local runner.Runner
	node  string
	poll  time.Duration
	now   func() time.Time
	// execPoll is how often a running `qm guest exec` is checked.
	execPoll time.Duration
}

// Detect reports whether the machine is a Proxmox VE node.
func Detect(ctx context.Context, local runner.Runner) bool {
	res, err := local.Run(ctx, runner.Command("test", "-d", "/etc/pve"))
	return err == nil && res.ExitCode == 0 && runner.Has(ctx, local, "pvesh") && runner.Has(ctx, local, "pct")
}

// New creates a provider. An empty node uses the short host name.
func New(ctx context.Context, local runner.Runner, node string, poll time.Duration) (*Provider, error) {
	if node == "" {
		res, err := runner.Check(ctx, local, runner.Command("hostname"))
		if err != nil {
			return nil, fmt.Errorf("finding the proxmox node name: %w", err)
		}
		node, _, _ = strings.Cut(strings.TrimSpace(res.Stdout), ".")
	}
	if node == "" || strings.ContainsAny(node, "/ ") {
		return nil, fmt.Errorf("invalid proxmox node name %q", node)
	}
	return &Provider{local: local, node: node, poll: poll, now: time.Now, execPoll: 2 * time.Second}, nil
}

// Platform implements guest.Provider.
func (*Provider) Platform() string { return "proxmox" }

// Node returns the node name.
func (p *Provider) Node() string { return p.node }

func (p *Provider) pvesh(ctx context.Context, path string, out any) error {
	res, err := runner.Check(ctx, p.local, runner.Command("pvesh", "get", path, "--output-format", "json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(res.Stdout), out); err != nil {
		return fmt.Errorf("parsing pvesh %s: %w", path, err)
	}
	return nil
}

// vmid accepts the VMID as a number or a string (the API returns both).
type vmid string

func (v *vmid) UnmarshalJSON(b []byte) error {
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*v = vmid(n.String())
	return nil
}

// flag accepts 0/1 as a number or a string.
type flag bool

func (f *flag) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	*f = s == "1" || s == "true"
	return nil
}

type listEntry struct {
	VMID     vmid   `json:"vmid"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Template flag   `json:"template"`
}

// Guests implements guest.Provider. Templates are skipped.
func (p *Provider) Guests(ctx context.Context) ([]guest.Guest, error) {
	var guests []guest.Guest
	for _, kind := range []string{kindLXC, kindQEMU} {
		var entries []listEntry
		if err := p.pvesh(ctx, "/nodes/"+p.node+"/"+kind, &entries); err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.Template {
				continue
			}
			g := guest.Guest{Platform: "proxmox", ID: string(e.VMID), Name: e.Name, Status: e.Status, Type: guest.Container}
			if g.Name == "" {
				g.Name = kind + "-" + g.ID
			}
			if kind == kindLXC {
				// The file instead of pvesh: this runs every poll for every
				// container, and pvesh is a Perl process each time.
				path := "/etc/pve/nodes/" + p.node + "/lxc/" + g.ID + ".conf"
				res, err := runner.Check(ctx, p.local, runner.Command("cat", path))
				if err != nil {
					return nil, err
				}
				g.App = App(ParseConfig(res.Stdout))
			}
			if kind == kindQEMU {
				g.Type = guest.VirtualMachine
				if g.Running() {
					enabled, err := p.agentEnabled(ctx, g.ID)
					if err != nil {
						return nil, err
					}
					if !enabled {
						g.Unmanageable = "the QEMU guest agent is not enabled for this VM"
					}
				}
			}
			guests = append(guests, g)
		}
	}
	slices.SortFunc(guests, func(a, b guest.Guest) int { return strings.Compare(a.Name, b.Name) })
	return guests, nil
}

// ParseConfig returns the entrypoint and notes (description) of a container
// from its configuration file. The notes are the comment lines at the top,
// with ':', '%' and unusual characters percent-encoded; snapshots and
// pending changes follow in [sections].
func ParseConfig(conf string) (entrypoint, notes string) {
	var b strings.Builder
	for _, line := range strings.Split(conf, "\n") {
		switch {
		case strings.HasPrefix(line, "["):
			return entrypoint, b.String()
		case strings.HasPrefix(line, "#"):
			text, err := url.PathUnescape(line[1:])
			if err != nil {
				text = line[1:]
			}
			b.WriteString(text + "\n")
		default:
			if key, value, ok := strings.Cut(line, ":"); ok && key == "entrypoint" {
				entrypoint = strings.TrimSpace(value)
			}
		}
	}
	return entrypoint, b.String()
}

// App describes an OCI application container from its entrypoint and notes;
// nil for system containers. Proxmox VE sets the entrypoint from the image
// when it is not /sbin/init, but records neither the image nor its digest,
// so they come from an "image: <reference>" line in the notes, optionally
// with "@sha256:..." for the digest of the running image.
func App(entrypoint, notes string) *guest.App {
	if entrypoint == "" || entrypoint == "/sbin/init" {
		return nil
	}
	app := &guest.App{ByDigest: true, NoImage: `add a line "image: <reference>" to the notes of the container`}
	for _, line := range strings.Split(notes, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "image") {
			continue
		}
		ref := strings.Trim(strings.TrimSpace(value), "`")
		if ref == "" || strings.ContainsAny(ref, " \t") {
			continue
		}
		app.Image, app.NoImage = ref, ""
		// "repo:tag@sha256:..." tracks the tag and runs that digest; a
		// reference pinned to a digest without a tag stays pinned.
		if name, digest, pinned := strings.Cut(ref, "@"); pinned {
			app.Fingerprint = digest
			if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
				app.Image = name
			}
		}
		break
	}
	return app
}

func (p *Provider) agentEnabled(ctx context.Context, id string) (bool, error) {
	var cfg struct {
		Agent string `json:"agent"`
	}
	if err := p.pvesh(ctx, "/nodes/"+p.node+"/qemu/"+id+"/config", &cfg); err != nil {
		return false, err
	}
	return AgentEnabled(cfg.Agent), nil
}

// AgentEnabled parses the "agent" property of a VM: "1", "1,fstrim_cloned_disks=1" or "enabled=1,...".
func AgentEnabled(value string) bool {
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "1" || part == "enabled=1" {
			return true
		}
	}
	return false
}

func kind(g guest.Guest) string {
	if g.Type == guest.VirtualMachine {
		return kindQEMU
	}
	return kindLXC
}

// tool is the per-kind command line tool.
func tool(g guest.Guest) string {
	if g.Type == guest.VirtualMachine {
		return "qm"
	}
	return "pct"
}

// Runner implements guest.Provider.
func (p *Provider) Runner(g guest.Guest) runner.Runner {
	if g.Type == guest.VirtualMachine {
		return &vmRunner{p: p, g: g}
	}
	return &ctRunner{p: p, g: g}
}

// withEnv wraps a command in `env` so the guest gets the agent's environment.
func withEnv(cmd runner.Cmd) []string {
	env := runner.Environment(cmd)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	args := []string{"env"}
	for _, k := range keys {
		args = append(args, k+"="+env[k])
	}
	return append(args, cmd.Args...)
}

type ctRunner struct {
	p *Provider
	g guest.Guest
}

// Run implements runner.Runner with `pct exec`, which passes the exit code on.
func (r *ctRunner) Run(ctx context.Context, cmd runner.Cmd) (runner.Result, error) {
	if len(cmd.Args) == 0 {
		return runner.Result{}, errors.New("empty command")
	}
	args := append([]string{"pct", "exec", r.g.ID, "--"}, withEnv(cmd)...)
	return r.p.local.Run(ctx, runner.Command(args...))
}

type vmRunner struct {
	p *Provider
	g guest.Guest
}

type execStatus struct {
	PID      int    `json:"pid"`
	Exited   flag   `json:"exited"`
	ExitCode int    `json:"exitcode"`
	OutData  string `json:"out-data"`
	ErrData  string `json:"err-data"`
}

// Run implements runner.Runner with the QEMU guest agent: start the command
// without waiting, then poll its status until it exits.
func (r *vmRunner) Run(ctx context.Context, cmd runner.Cmd) (runner.Result, error) {
	if len(cmd.Args) == 0 {
		return runner.Result{}, errors.New("empty command")
	}
	args := append([]string{"qm", "guest", "exec", r.g.ID, "--timeout", "0", "--"}, withEnv(cmd)...)
	res, err := runner.Check(ctx, r.p.local, runner.Command(args...))
	if err != nil {
		return runner.Result{}, fmt.Errorf("guest agent of VM %s: %w", r.g.ID, err)
	}
	var started execStatus
	if err := json.Unmarshal([]byte(res.Stdout), &started); err != nil || started.PID == 0 {
		return runner.Result{}, fmt.Errorf("unexpected answer from the guest agent of VM %s: %q", r.g.ID, res.Stdout)
	}
	pid := strconv.Itoa(started.PID)
	for {
		res, err := runner.Check(ctx, r.p.local, runner.Command("qm", "guest", "exec-status", r.g.ID, pid))
		if err != nil {
			return runner.Result{}, fmt.Errorf("guest agent of VM %s: %w", r.g.ID, err)
		}
		var st execStatus
		if err := json.Unmarshal([]byte(res.Stdout), &st); err != nil {
			return runner.Result{}, fmt.Errorf("parsing exec-status of VM %s: %w", r.g.ID, err)
		}
		if st.Exited {
			return runner.Result{Stdout: st.OutData, Stderr: st.ErrData, ExitCode: st.ExitCode}, nil
		}
		select {
		case <-ctx.Done():
			return runner.Result{}, ctx.Err()
		case <-time.After(r.p.execPoll):
		}
	}
}

// SnapshotDescription marks agent snapshots and records when they expire,
// because Proxmox has no snapshot expiry of its own.
const SnapshotDescription = "linux-ha-agent pre-upgrade snapshot; expires "

// CreateSnapshot implements guest.Provider. Expired snapshots taken by the
// agent are removed afterwards.
func (p *Provider) CreateSnapshot(ctx context.Context, g guest.Guest, name string, expires time.Time) error {
	desc := SnapshotDescription + expires.UTC().Format(time.RFC3339)
	if _, err := runner.Check(ctx, p.local, runner.Command(tool(g), "snapshot", g.ID, name, "--description", desc)); err != nil {
		return fmt.Errorf("snapshot of %s: %w", g.Name, err)
	}
	return p.pruneSnapshots(ctx, g)
}

type snapshotEntry struct {
	Name        string `json:"name"`
	SnapTime    int64  `json:"snaptime"`
	Description string `json:"description"`
}

func (p *Provider) snapshotEntries(ctx context.Context, g guest.Guest) ([]snapshotEntry, error) {
	var entries []snapshotEntry
	if err := p.pvesh(ctx, "/nodes/"+p.node+"/"+kind(g)+"/"+g.ID+"/snapshot", &entries); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(entries, func(e snapshotEntry) bool { return e.Name == "current" }), nil
}

func (p *Provider) pruneSnapshots(ctx context.Context, g guest.Guest) error {
	entries, err := p.snapshotEntries(ctx, g)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		expiry, ok := strings.CutPrefix(strings.TrimSpace(e.Description), SnapshotDescription)
		if !ok {
			continue
		}
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(expiry))
		if err != nil || t.After(p.now()) {
			continue
		}
		if _, err := runner.Check(ctx, p.local, runner.Command(tool(g), "delsnapshot", g.ID, e.Name)); err != nil {
			errs = append(errs, fmt.Errorf("removing expired snapshot %s of %s: %w", e.Name, g.Name, err))
		}
	}
	return errors.Join(errs...)
}

// Snapshots implements guest.Provider.
func (p *Provider) Snapshots(ctx context.Context, g guest.Guest) ([]guest.Snapshot, error) {
	entries, err := p.snapshotEntries(ctx, g)
	if err != nil {
		return nil, err
	}
	snaps := make([]guest.Snapshot, 0, len(entries))
	for _, e := range entries {
		snaps = append(snaps, guest.Snapshot{Name: e.Name, CreatedAt: time.Unix(e.SnapTime, 0)})
	}
	slices.SortFunc(snaps, func(a, b guest.Snapshot) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return snaps, nil
}

// RestoreSnapshot implements guest.Provider. A running guest is started
// again after the rollback.
func (p *Provider) RestoreSnapshot(ctx context.Context, g guest.Guest, name string) error {
	args := []string{tool(g), "rollback", g.ID, name}
	if g.Running() {
		args = append(args, "--start", "1")
	}
	if _, err := runner.Check(ctx, p.local, runner.Command(args...)); err != nil {
		return fmt.Errorf("restore of %s to %s: %w", g.Name, name, err)
	}
	return nil
}

// Restart implements guest.Provider.
func (p *Provider) Restart(ctx context.Context, g guest.Guest, timeout time.Duration) error {
	secs := strconv.Itoa(int(timeout.Seconds()))
	if _, err := runner.Check(ctx, p.local, runner.Command(tool(g), "reboot", g.ID, "--timeout", secs)); err != nil {
		return fmt.Errorf("restart of %s: %w", g.Name, err)
	}
	return nil
}

// WatchLifecycle implements guest.Provider by polling: Proxmox has no local
// event stream for guest state changes.
func (p *Provider) WatchLifecycle(ctx context.Context, fn func()) error {
	last := ""
	ticker := time.NewTicker(p.poll)
	defer ticker.Stop()
	for {
		guests, err := p.Guests(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		var b strings.Builder
		for _, g := range guests {
			fmt.Fprintf(&b, "%s/%s/%s/%s;", g.ID, g.Name, g.Status, g.Unmanageable)
			if g.App != nil {
				// Notes edited: the app is checked against its new image.
				fmt.Fprintf(&b, "%s@%s;", g.App.Image, g.App.Fingerprint)
			}
		}
		if last != "" && b.String() != last {
			fn()
		}
		last = b.String()
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
