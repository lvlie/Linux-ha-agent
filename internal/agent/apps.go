package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/lvlie/linux-ha-agent/internal/containers"
	"github.com/lvlie/linux-ha-agent/internal/guest"
	"github.com/lvlie/linux-ha-agent/internal/incuscompose"
)

// AppCheck is the image check of an OCI application container.
type AppCheck struct {
	Image string `json:"image"`
	// Installed and Latest are Incus image fingerprints.
	Installed string `json:"installed"`
	Latest    string `json:"latest"`
	// Error explains why the registry could not be asked.
	Error string `json:"error,omitempty"`
	// ComposeDir is the directory of the incus-compose project, if found.
	ComposeDir string `json:"compose_dir,omitempty"`
}

// UpdateAvailable reports whether the registry has a newer image.
func (c *AppCheck) UpdateAvailable() bool {
	return c != nil && c.Latest != "" && c.Installed != "" && c.Latest != c.Installed
}

func (d *Device) app() *guest.App {
	if d.Guest == nil {
		return nil
	}
	return d.Guest.App
}

// appCheck compares the image of an application container with its registry.
// It needs nothing from inside the container, so it also works when stopped.
func (a *Agent) appCheck(ctx context.Context, d *Device) {
	a.mu.Lock()
	app := *d.app()
	a.mu.Unlock()
	c := &Check{Time: a.now(), App: &AppCheck{Image: app.Image, Installed: app.Fingerprint}}
	resolver, ok := a.registry.(containers.FingerprintResolver)
	switch {
	case app.Image == "":
		c.App.Error = "the instance has no image information"
	case !a.cfg.Checks.ContainerImages || !ok:
		c.App.Error = "image checks are off"
	default:
		latest, err := resolver.Fingerprint(ctx, app.Image, app.Architecture)
		if err != nil {
			c.App.Error = err.Error()
		}
		c.App.Latest = latest
	}
	if app.ComposeProject != "" {
		c.App.ComposeDir, _ = a.composeDir(app.ComposeProject)
	}
	c.Error = c.App.Error
	a.mu.Lock()
	d.Check = c
	a.mu.Unlock()
	a.publish(d, false)
}

// composeDir finds the directory of an incus-compose project.
func (a *Agent) composeDir(project string) (string, bool) {
	dir, ok := incuscompose.Find(a.cfg.Incus.ComposeDirs)[project]
	return dir, ok
}

// appInstallable reports whether the agent can update an app's image.
// Callers hold a.mu.
func (a *Agent) appInstallable(d *Device) bool {
	app := d.app()
	switch {
	case app == nil || app.Image == "":
		return false
	case app.ComposeProject != "":
		return d.Check != nil && d.Check.App != nil && d.Check.App.ComposeDir != ""
	default:
		_, ok := d.Provider.(guest.AppProvider)
		return ok
	}
}

// upgradeApp updates an application container to the newest version of its
// image: through incus-compose when it manages the container, otherwise by
// rebuilding the instance from the registry.
func (a *Agent) upgradeApp(ctx context.Context, d *Device, report *RunReport) {
	a.mu.Lock()
	g := *d.Guest
	a.mu.Unlock()
	app := g.App
	if app.Image == "" {
		report.step("image", false, errors.New("the instance has no image information"), "")
		return
	}

	if app.ComposeProject != "" {
		dir, ok := a.composeDir(app.ComposeProject)
		if !ok {
			report.step("incus-compose", false, fmt.Errorf("project %q not found in incus.compose_dirs %v", app.ComposeProject, a.cfg.Incus.ComposeDirs), "")
			return
		}
		// incus-compose recreates the instance, which removes its snapshots,
		// so none is taken; volumes are kept.
		err := incuscompose.Upgrade(ctx, a.local, dir, app.ComposeService)
		changed := a.refreshGuest(ctx, d, app.Fingerprint)
		report.step(fmt.Sprintf("incus-compose %s/%s", app.ComposeProject, app.ComposeService), changed, err, imageDetail(changed, app.Image))
		return
	}

	rebuilder, ok := d.Provider.(guest.AppProvider)
	if !ok {
		report.step("image", false, fmt.Errorf("%s cannot rebuild application containers", g.Platform), "")
		return
	}
	if resolver, ok := a.registry.(containers.FingerprintResolver); ok {
		latest, err := resolver.Fingerprint(ctx, app.Image, app.Architecture)
		if err == nil && latest == app.Fingerprint {
			report.step("image", false, nil, "up to date")
			return
		}
	}
	// Incus cannot rebuild an instance that has snapshots, so none is taken
	// (the data of application containers belongs in volumes, which a
	// rebuild keeps), and the user's own snapshots are left alone.
	if snaps, err := d.Provider.Snapshots(ctx, g); err == nil && len(snaps) > 0 {
		report.step("rebuild from "+app.Image, false, fmt.Errorf("the instance has %d snapshot(s), and Incus cannot rebuild an instance with snapshots; remove them to update it", len(snaps)), "")
		return
	}
	err := rebuilder.RebuildApp(ctx, g, a.cfg.Guests.StopTimeout)
	changed := a.refreshGuest(ctx, d, app.Fingerprint)
	report.step("rebuild from "+app.Image, changed, err, imageDetail(changed, app.Image))
}

func imageDetail(changed bool, image string) string {
	if changed {
		return "now runs the newest " + image
	}
	return "up to date"
}

// refreshGuest reloads the guest from its platform and reports whether its
// image changed from before.
func (a *Agent) refreshGuest(ctx context.Context, d *Device, before string) bool {
	guests, err := d.Provider.Guests(ctx)
	if err != nil {
		return false
	}
	for _, g := range guests {
		if g.Key() != d.Guest.Key() {
			continue
		}
		a.mu.Lock()
		d.Guest = &g
		d.Runner = d.Provider.Runner(g)
		a.mu.Unlock()
		return g.App != nil && g.App.Fingerprint != before
	}
	return false
}
