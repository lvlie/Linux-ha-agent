// Package agent ties everything together: it keeps one Device per managed
// machine, checks and upgrades them one operation at a time, and publishes
// their state to Home Assistant over MQTT.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/lvlie/linux-ha-agent/internal/config"
	"github.com/lvlie/linux-ha-agent/internal/containers"
	"github.com/lvlie/linux-ha-agent/internal/guest"
	"github.com/lvlie/linux-ha-agent/internal/ha"
	"github.com/lvlie/linux-ha-agent/internal/mqtt"
	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// ProjectURL is shown in Home Assistant as the origin of the entities.
const ProjectURL = "https://github.com/lvlie/linux-ha-agent"

// Options configure an Agent.
type Options struct {
	Config config.Config
	MQTT   mqtt.Client
	// Providers manage the guests of virtualisation platforms on this host
	// (Incus, Proxmox VE). Empty on machines without guests.
	Providers []guest.Provider
	Local     runner.Runner
	Registry  containers.DigestResolver
	Hostname  string
	Version   string
	Logger    *slog.Logger
	Now       func() time.Time
	// RebootHost reboots the machine; defaults to `systemctl reboot`.
	RebootHost func(ctx context.Context) error
}

// Agent manages the host and its Incus guests.
type Agent struct {
	cfg       config.Config
	mqtt      mqtt.Client
	providers []guest.Provider
	local     runner.Runner
	registry  containers.DigestResolver
	version   string
	log       *slog.Logger
	now       func() time.Time
	reboot    func(ctx context.Context) error
	topics    ha.Topics
	schedule  cron.Schedule

	mu      sync.Mutex
	devices map[string]*Device
	host    *Device
	state   persisted
	pending map[string]bool
	ops     chan op
	idle    *sync.Cond
	busy    int
}

type op struct {
	kind    string
	device  string
	trigger string
}

func (o op) key() string { return o.kind + ":" + o.device }

// New creates an agent. Nothing happens until Run.
func New(opts Options) (*Agent, error) {
	if opts.MQTT == nil || opts.Local == nil {
		return nil, errors.New("agent needs an MQTT client and a local runner")
	}
	a := &Agent{
		cfg:       opts.Config,
		mqtt:      opts.MQTT,
		providers: opts.Providers,
		local:     opts.Local,
		registry:  opts.Registry,
		version:   opts.Version,
		log:       opts.Logger,
		now:       opts.Now,
		reboot:    opts.RebootHost,
		devices:   map[string]*Device{},
		pending:   map[string]bool{},
		ops:       make(chan op, 256),
	}
	a.idle = sync.NewCond(&a.mu)
	if a.log == nil {
		a.log = slog.Default()
	}
	if a.now == nil {
		a.now = time.Now
	}
	if a.version == "" {
		a.version = "dev"
	}
	if a.reboot == nil {
		a.reboot = func(ctx context.Context) error {
			_, err := runner.Check(ctx, a.local, runner.Command("systemctl", "reboot"))
			return err
		}
	}
	name := opts.Config.Host.Name
	if name == "" {
		name = opts.Hostname
	}
	if name == "" {
		return nil, errors.New("no host name: set host.name")
	}
	hostID := HostID(name)
	a.topics = ha.Topics{DiscoveryPrefix: a.cfg.HomeAssistant.DiscoveryPrefix, Base: a.cfg.HomeAssistant.BaseTopic, HostID: hostID}
	if a.cfg.Upgrade.Schedule != "" {
		s, err := config.ParseSchedule(a.cfg.Upgrade.Schedule)
		if err != nil {
			return nil, err
		}
		a.schedule = s
	}
	st, err := loadState(a.cfg.StateFile)
	if err != nil {
		a.log.Warn("ignoring unreadable state file", "err", err)
	}
	a.state = st
	a.host = &Device{ID: hostID, Name: name, Host: true, Runner: a.local, LastRun: st.LastRuns[hostID]}
	a.devices[hostID] = a.host
	return a, nil
}

// HostID is the device ID of a host with the given name.
func HostID(name string) string { return ha.Slug(name) }

// Reconnected republishes availability, discovery and state after the MQTT
// connection was (re)established.
func (a *Agent) Reconnected() {
	if err := a.mqtt.Publish(a.topics.Availability(), true, []byte("online")); err != nil {
		a.log.Error("publishing availability failed", "err", err)
	}
	a.PublishAll(true)
}

// Topics exposes the MQTT topics (for tests and logs).
func (a *Agent) Topics() ha.Topics { return a.topics }

func (a *Agent) autoUpgrade() bool {
	if a.state.AutoUpgrade != nil {
		return *a.state.AutoUpgrade
	}
	return a.cfg.Upgrade.AutoUpgrade
}

func (a *Agent) features() ha.Features {
	return ha.Features{
		Upgrade:  a.cfg.Permissions.Upgrade,
		Reboot:   a.cfg.Permissions.Reboot,
		Restore:  a.cfg.Permissions.Restore && a.cfg.Guests.Snapshot && len(a.providers) > 0,
		Schedule: a.schedule != nil,
	}
}

// Start subscribes, publishes the initial state and starts the worker. It
// returns once everything is running; Run blocks instead.
func (a *Agent) Start(ctx context.Context) error {
	if err := a.mqtt.Subscribe(a.topics.CommandFilter(), a.handleCommand); err != nil {
		return err
	}
	if err := a.mqtt.Subscribe(a.topics.AutoUpgrade(), a.handleAutoUpgrade); err != nil {
		return err
	}
	if err := a.mqtt.Subscribe(a.topics.HAStatus(), func(_ string, payload []byte) {
		if string(payload) == "online" {
			a.log.Info("home assistant restarted, republishing discovery")
			a.PublishAll(true)
		}
	}); err != nil {
		return err
	}
	if err := a.mqtt.Publish(a.topics.Availability(), true, []byte("online")); err != nil {
		return err
	}
	a.publish(a.host, false)

	go a.worker(ctx)
	if len(a.providers) > 0 {
		a.enqueue(op{kind: "sync"})
	}
	a.enqueue(op{kind: "check_all", trigger: "startup"})
	return nil
}

// Run starts the agent and blocks until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.Start(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(a.cfg.Checks.Interval)
	defer ticker.Stop()

	var scheduler *cron.Cron
	if a.schedule != nil {
		scheduler = cron.New()
		scheduler.Schedule(a.schedule, cron.FuncJob(func() {
			if a.autoUpgrade() {
				a.log.Info("scheduled upgrade starting")
				a.enqueue(op{kind: ha.CmdUpgradeAll, trigger: "schedule"})
			} else {
				a.log.Info("scheduled upgrade skipped: automatic upgrades are off")
			}
			a.publish(a.host, false) // next run time changed
		}))
		scheduler.Start()
		defer scheduler.Stop()
	}
	for _, p := range a.providers {
		go a.watchGuests(ctx, p)
	}
	for {
		select {
		case <-ctx.Done():
			_ = a.mqtt.Publish(a.topics.Availability(), true, []byte("offline"))
			return nil
		case <-ticker.C:
			a.enqueue(op{kind: "check_all", trigger: "interval"})
		}
	}
}

func (a *Agent) watchGuests(ctx context.Context, p guest.Provider) {
	for ctx.Err() == nil {
		err := p.WatchLifecycle(ctx, func() { a.enqueue(op{kind: "sync"}) })
		if ctx.Err() != nil {
			return
		}
		a.log.Warn("watching guests failed, retrying", "platform", p.Platform(), "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
		a.enqueue(op{kind: "sync"})
	}
}

// enqueue adds an operation unless an identical one is already waiting.
func (a *Agent) enqueue(o op) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pending[o.key()] {
		return
	}
	select {
	case a.ops <- o:
		a.pending[o.key()] = true
		a.busy++
	default:
		a.log.Warn("operation queue full, dropping", "op", o.kind, "device", o.device)
	}
}

// WaitIdle blocks until the queue is empty and no operation runs (tests).
func (a *Agent) WaitIdle() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for a.busy > 0 {
		a.idle.Wait()
	}
}

func (a *Agent) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case o := <-a.ops:
			a.mu.Lock()
			delete(a.pending, o.key())
			a.mu.Unlock()
			a.execute(ctx, o)
			a.mu.Lock()
			a.busy--
			if a.busy == 0 {
				a.idle.Broadcast()
			}
			a.mu.Unlock()
		}
	}
}

func (a *Agent) execute(ctx context.Context, o op) {
	defer func() {
		if r := recover(); r != nil {
			a.log.Error("operation panicked", "op", o.kind, "device", o.device, "panic", r)
		}
	}()
	if o.kind == "sync" {
		a.syncGuests(ctx)
		return
	}
	if o.kind == "check_all" {
		for _, d := range a.deviceList() {
			a.enqueue(op{kind: ha.CmdCheck, device: d.ID, trigger: o.trigger})
		}
		return
	}
	if o.kind == ha.CmdUpgradeAll {
		a.upgradeAll(ctx, o.trigger)
		return
	}
	d := a.device(o.device)
	if d == nil {
		a.log.Warn("operation for unknown device", "op", o.kind, "device", o.device)
		return
	}
	switch o.kind {
	case ha.CmdCheck:
		a.check(ctx, d)
	case ha.CmdUpgrade, ha.CmdUpgradePackages, ha.CmdUpgradeContainers:
		needsReboot := a.upgrade(ctx, d, o.kind, o.trigger)
		if needsReboot && d.Host && a.cfg.Upgrade.RebootHost {
			a.rebootHost(ctx, o.trigger)
		}
	case ha.CmdReboot:
		if d.Host {
			a.rebootHost(ctx, o.trigger)
		} else {
			a.restartGuest(ctx, d, o.trigger)
		}
	case ha.CmdRestore:
		a.restore(ctx, d, o.trigger)
	}
}

func (a *Agent) device(id string) *Device {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.devices[id]
}

// deviceList returns the host first, then guests by name.
func (a *Agent) deviceList() []*Device {
	a.mu.Lock()
	defer a.mu.Unlock()
	list := make([]*Device, 0, len(a.devices))
	for _, d := range a.devices {
		if !d.Host {
			list = append(list, d)
		}
	}
	slices.SortFunc(list, func(x, y *Device) int { return strings.Compare(x.ID, y.ID) })
	return append([]*Device{a.host}, list...)
}

// handleCommand validates and queues a command from Home Assistant.
func (a *Agent) handleCommand(topic string, payload []byte) {
	parts := strings.Split(topic, "/")
	if len(parts) < 2 {
		return
	}
	deviceID := parts[len(parts)-2]
	cmd := strings.TrimSpace(string(payload))
	d := a.device(deviceID)
	log := a.log.With("device", deviceID, "command", cmd)
	if d == nil {
		log.Warn("command rejected: unknown device")
		return
	}
	if err := a.allowed(d, cmd); err != nil {
		log.Warn("command rejected", "reason", err)
		return
	}
	log.Info("command accepted")
	a.enqueue(op{kind: cmd, device: d.ID, trigger: "home assistant"})
}

func (a *Agent) allowed(d *Device, cmd string) error {
	p := a.cfg.Permissions
	switch cmd {
	case ha.CmdCheck:
		return nil
	case ha.CmdUpgrade, ha.CmdUpgradePackages, ha.CmdUpgradeContainers:
		if !p.Upgrade {
			return errors.New("upgrades are not permitted")
		}
	case ha.CmdUpgradeAll:
		if !p.Upgrade {
			return errors.New("upgrades are not permitted")
		}
		if !d.Host {
			return errors.New("upgrade_all is only available on the host")
		}
	case ha.CmdReboot:
		if !p.Reboot {
			return errors.New("reboots are not permitted")
		}
	case ha.CmdRestore:
		if !a.features().Restore {
			return errors.New("snapshot restores are not permitted")
		}
		if d.Guest == nil {
			return errors.New("only guests can be restored")
		}
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
	return nil
}

func (a *Agent) handleAutoUpgrade(_ string, payload []byte) {
	if a.schedule == nil {
		return
	}
	on := strings.EqualFold(strings.TrimSpace(string(payload)), "ON")
	a.mu.Lock()
	a.state.AutoUpgrade = &on
	a.mu.Unlock()
	a.log.Info("automatic upgrades switched", "enabled", on)
	a.saveState()
	a.publish(a.host, false)
}

func (a *Agent) saveState() {
	a.mu.Lock()
	st := persisted{AutoUpgrade: a.state.AutoUpgrade, LastRuns: map[string]*RunReport{}}
	for id, d := range a.devices {
		st.Devices = append(st.Devices, id)
		if d.LastRun != nil {
			st.LastRuns[id] = d.LastRun
		}
	}
	a.mu.Unlock()
	slices.Sort(st.Devices)
	if err := saveState(a.cfg.StateFile, st); err != nil {
		a.log.Error("saving state failed", "err", err)
	}
}

// PublishAll republishes every device; force resends discovery too.
func (a *Agent) PublishAll(force bool) {
	for _, d := range a.deviceList() {
		a.publish(d, force)
	}
}

// publish sends a device's discovery (when changed) and state.
func (a *Agent) publish(d *Device, forceDiscovery bool) {
	a.mu.Lock()
	payload, err := ha.Payload(a.topics, ha.Origin{Name: "linux-ha-agent", Version: a.version, URL: ProjectURL}, d.haDevice(a.topics.HostID), a.features())
	sendDiscovery := err == nil && (forceDiscovery || string(payload) != d.discovery)
	if sendDiscovery {
		d.discovery = string(payload)
	}
	var extra map[string]any
	if d.Host {
		extra = map[string]any{
			"agent_version": a.version,
			"guests":        len(a.devices) - 1,
			"auto_upgrade":  a.autoUpgrade(),
			"next_upgrade":  nil,
		}
		if a.schedule != nil && a.autoUpgrade() {
			extra["next_upgrade"] = a.schedule.Next(a.now()).Format(time.RFC3339)
		}
	}
	state, stateErr := json.Marshal(d.state(extra))
	a.mu.Unlock()

	if err != nil || stateErr != nil {
		a.log.Error("building mqtt payload failed", "device", d.ID, "err", errors.Join(err, stateErr))
		return
	}
	if sendDiscovery {
		if err := a.mqtt.Publish(a.topics.Config(d.ID), true, payload); err != nil {
			a.log.Error("publishing discovery failed", "device", d.ID, "err", err)
		}
	}
	if err := a.mqtt.Publish(a.topics.State(d.ID), true, state); err != nil {
		a.log.Error("publishing state failed", "device", d.ID, "err", err)
	}
}

// unpublish removes a device from Home Assistant.
func (a *Agent) unpublish(id string) {
	_ = a.mqtt.Publish(a.topics.Config(id), true, nil)
	_ = a.mqtt.Publish(a.topics.State(id), true, nil)
}

func (a *Agent) setOperation(d *Device, operation string) {
	a.mu.Lock()
	d.Operation = operation
	a.mu.Unlock()
	a.publish(d, false)
}

func (a *Agent) managed(g guest.Guest) bool {
	if !a.cfg.Guests.Includes(g.Type) || a.cfg.Guests.Excluded(g.Project, g.Name, g.ID) {
		return false
	}
	return g.Platform != "incus" || a.cfg.Incus.InProject(g.Project)
}

// syncGuests reconciles devices with the guests the platforms report.
func (a *Agent) syncGuests(ctx context.Context) {
	type found struct {
		g guest.Guest
		p guest.Provider
	}
	var all []found
	for _, p := range a.providers {
		guests, err := p.Guests(ctx)
		if err != nil {
			// Keep the current devices rather than removing them all.
			a.log.Error("listing guests failed", "platform", p.Platform(), "err", err)
			return
		}
		for _, g := range guests {
			if a.managed(g) {
				all = append(all, found{g, p})
			}
		}
	}
	seen := map[string]bool{a.host.ID: true}
	var added, started []*Device
	a.mu.Lock()
	for _, f := range all {
		g := f.g
		id := guestID(a.topics.HostID, g)
		if seen[id] {
			a.log.Warn("two guests map to the same device, ignoring the second", "device", id, "platform", g.Platform)
			continue
		}
		seen[id] = true
		d := a.devices[id]
		if d == nil {
			d = &Device{ID: id, Name: guestName(g), Guest: &g, Provider: f.p, Runner: f.p.Runner(g), LastRun: a.state.LastRuns[id]}
			a.devices[id] = d
			added = append(added, d)
			continue
		}
		wasRunning := d.running()
		d.Guest = &g
		d.Provider = f.p
		d.Runner = f.p.Runner(g)
		if !wasRunning && g.Running() {
			started = append(started, d)
		}
	}
	var removed []string
	for id := range a.devices {
		if !seen[id] {
			delete(a.devices, id)
			removed = append(removed, id)
		}
	}
	// Devices from before a restart that no longer exist.
	for _, id := range a.state.Devices {
		if !seen[id] && !slices.Contains(removed, id) {
			removed = append(removed, id)
		}
	}
	a.state.Devices = nil
	a.mu.Unlock()

	for _, id := range removed {
		a.log.Info("guest gone, removing from home assistant", "device", id)
		a.unpublish(id)
	}
	for _, d := range added {
		a.log.Info("guest found", "device", d.ID, "status", d.Guest.Status)
		if d.running() {
			a.enqueue(op{kind: ha.CmdCheck, device: d.ID, trigger: "discovered"})
		}
	}
	for _, d := range started {
		a.enqueue(op{kind: ha.CmdCheck, device: d.ID, trigger: "started"})
	}
	a.PublishAll(false)
	if len(added) > 0 || len(removed) > 0 {
		a.saveState()
	}
}

func guestID(hostID string, g guest.Guest) string {
	if g.Project == "" || g.Project == "default" {
		return ha.Slug(hostID, g.Name)
	}
	return ha.Slug(hostID, g.Project, g.Name)
}

func guestName(g guest.Guest) string {
	if g.Project == "" || g.Project == "default" {
		return g.Name
	}
	return g.Project + "/" + g.Name
}
