package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/lvlie/linux-ha-agent/internal/commands"
	"github.com/lvlie/linux-ha-agent/internal/config"
	"github.com/lvlie/linux-ha-agent/internal/ha"
)

// commandTick is how often the command loop looks for commands that are due.
const commandTick = 10 * time.Second

// runsOn reports whether commands for these machines run on a device.
func runsOn(machines []string, d *Device) bool {
	if d.Guest == nil {
		return config.RunsOn(machines, nil)
	}
	ref := guestRef(*d.Guest)
	return config.RunsOn(machines, &ref)
}

// commandEntities are the configured commands that run on a device.
func (a *Agent) commandEntities(d *Device) []ha.Command {
	var list []ha.Command
	for _, c := range a.cfg.Commands.Expose {
		if runsOn(c.Machines, d) {
			list = append(list, ha.Command{Key: c.Key(), Name: c.Name, Binary: c.Type == "binary_sensor",
				Unit: c.Unit, Icon: c.Icon, DeviceClass: c.DeviceClass})
		}
	}
	return list
}

// anyCommand reports whether arbitrary commands may run on a device.
func (a *Agent) anyCommand(d *Device) bool {
	return a.cfg.Commands.AllowAny && runsOn(a.cfg.Commands.AllowAnyMachines, d)
}

// canRun reports whether commands can run on a device right now.
func canRun(d *Device) bool {
	return d.running() && d.manageable()
}

// commandLoop runs the configured commands when they are due. It runs
// outside the operation queue, so a long upgrade does not stop the sensors;
// the commands cannot change anything.
func (a *Agent) commandLoop(ctx context.Context) {
	ticker := time.NewTicker(commandTick)
	defer ticker.Stop()
	for {
		a.runDueCommands(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runDueCommands runs every configured command whose interval has passed,
// one at a time, and publishes the devices whose results changed.
func (a *Agent) runDueCommands(ctx context.Context) {
	for _, d := range a.deviceList() {
		changed := false
		for _, c := range a.cfg.Commands.Expose {
			if ctx.Err() != nil {
				return
			}
			a.mu.Lock()
			due := canRun(d) && runsOn(c.Machines, d) && a.now().Sub(d.commandsRun[c.Key()]) >= c.Interval
			r := d.Runner
			a.mu.Unlock()
			if !due {
				continue
			}
			res := commands.Run(ctx, r, c.Command, a.cfg.Commands.Timeout, a.now)
			if res.Error != "" {
				a.log.Debug("command failed", "device", d.ID, "command", c.Name, "err", res.Error)
			}
			a.mu.Lock()
			if d.commands == nil {
				d.commands, d.commandsRun = map[string]commands.Result{}, map[string]time.Time{}
			}
			d.commands[c.Key()] = res
			d.commandsRun[c.Key()] = a.now()
			a.mu.Unlock()
			changed = true
		}
		if changed {
			a.publish(d, false)
		}
	}
}

// runAnyCommand runs a command typed in Home Assistant, one at a time per
// device.
func (a *Agent) runAnyCommand(d *Device, shell string, log *slog.Logger) {
	a.mu.Lock()
	if d.commandRunning || !canRun(d) {
		a.mu.Unlock()
		log.Warn("command rejected: another command is running or the machine is not running")
		return
	}
	d.commandRunning = true
	r := d.Runner
	a.mu.Unlock()
	log.Info("running command as nobody")
	go func() {
		res := commands.Run(context.Background(), r, shell, a.cfg.Commands.Timeout, a.now)
		a.mu.Lock()
		d.lastCommand = &res
		d.commandRunning = false
		a.mu.Unlock()
		log.Info("command finished", "exit_code", res.ExitCode, "error", res.Error)
		a.publish(d, false)
	}()
}
