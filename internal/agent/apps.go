package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/lvlie/linux-ha-agent/internal/containers"
	"github.com/lvlie/linux-ha-agent/internal/guest"
	"github.com/lvlie/linux-ha-agent/internal/incuscompose"
)

// composeCacheTTL is how long the incus-compose projects found in
// incus.compose_dirs are reused before the directories are read again.
const composeCacheTTL = time.Minute

// AppCheck is the image check of an OCI application container.
type AppCheck struct {
	Image string `json:"image"`
	// Installed and Latest are Incus image fingerprints. Installed is the
	// image at check time; the guest's current one may differ since.
	Installed string `json:"installed"`
	Latest    string `json:"latest"`
	// LatestDigest is the registry digest of Latest.
	LatestDigest string `json:"latest_digest,omitempty"`
	// Error explains why the registry could not be asked.
	Error string `json:"error,omitempty"`
	// ComposeDir is the directory of the incus-compose project, if found.
	ComposeDir string `json:"compose_dir,omitempty"`
}

// UpdateAvailable reports whether the registry has a newer image than the
// installed one: the guest's current fingerprint, which changes without a new
// check when the container is updated outside the agent.
func (c *AppCheck) UpdateAvailable(installed string) bool {
	return c != nil && c.Latest != "" && installed != "" && c.Latest != installed
}

// appUpdate reports whether an application container has a newer image.
// Callers hold a.mu.
func (d *Device) appUpdate() bool {
	app := d.app()
	return app != nil && d.Check != nil && d.Check.App.UpdateAvailable(app.Fingerprint)
}

// app is the guest's application container, if it is one. Where the
// platform does not know the digest of the running image, it is the one
// assumed at the first check.
func (d *Device) app() *guest.App {
	if d.Guest == nil || d.Guest.App == nil {
		return nil
	}
	app := d.Guest.App
	if app.ByDigest && app.Fingerprint == "" {
		if digest := d.imageDigests[assumedKey(app.Image)]; digest != "" {
			filled := *app
			filled.Fingerprint = digest
			return &filled
		}
	}
	return app
}

// assumedKey is the Device.imageDigests key of the digest assumed for the
// running image of an application container on a platform that does not
// record it; per image reference, so that it is assumed anew when the
// reference changes.
func assumedKey(image string) string { return "assumed " + image }

// noImage explains why an application container cannot be checked.
func noImage(app guest.App) string {
	if app.NoImage != "" {
		return app.NoImage
	}
	return "the instance has no image information"
}

// appCheck compares the image of an application container with its registry.
// It needs nothing from inside the container, so it also works when stopped.
func (a *Agent) appCheck(ctx context.Context, d *Device) {
	a.mu.Lock()
	current := d.app()
	if current == nil {
		// Replaced by a guest that is no application container.
		a.mu.Unlock()
		a.publish(d, false)
		return
	}
	app := *current
	a.mu.Unlock()
	c := &Check{Time: a.now(), App: &AppCheck{Image: app.Image, Installed: app.Fingerprint}}
	resolver, ok := a.registry.(containers.FingerprintResolver)
	switch {
	case app.Image == "":
		c.App.Error = noImage(app)
	case app.ByDigest && a.cfg.Checks.ContainerImages && a.registry != nil:
		latest, err := a.registry.Digest(ctx, app.Image)
		if err != nil {
			c.App.Error = err.Error()
		}
		c.App.Latest, c.App.LatestDigest = latest, latest
	case app.ByDigest || !a.cfg.Checks.ContainerImages || !ok:
		c.App.Error = "image checks are off"
	default:
		latest, err := resolver.Fingerprint(ctx, app.Image, app.Architecture)
		if err != nil {
			c.App.Error = err.Error()
		}
		c.App.Latest = latest
		if err == nil {
			// Cached by Fingerprint: no second request.
			c.App.LatestDigest, _ = a.registry.Digest(ctx, app.Image)
		}
	}
	if app.ComposeProject != "" {
		c.App.ComposeDir, _ = a.composeDir(app.ComposeProject)
	}
	c.Error = c.App.Error
	a.mu.Lock()
	d.Check = c
	changed := d.rememberDigest(c.App.Latest, c.App.LatestDigest)
	if app.ByDigest && app.Fingerprint == "" && c.App.LatestDigest != "" {
		// The platform does not say which version runs: assume the newest.
		d.setImageDigest(assumedKey(app.Image), c.App.LatestDigest)
		changed = true
	}
	a.mu.Unlock()
	if changed {
		a.saveState()
	}
	a.publish(d, false)
}

// rememberDigest records the registry digest of the newest image of an
// application container. Incus keeps only the fingerprint, so this is how
// the digest of the running image is known: it was the newest when the
// container was created or updated. Only the running and the newest image
// are kept. It reports whether anything changed. Callers hold a.mu.
func (d *Device) rememberDigest(fingerprint, digest string) bool {
	app := d.app()
	if app == nil || fingerprint == "" || digest == "" || d.imageDigests[fingerprint] == digest {
		return false
	}
	keep := map[string]string{fingerprint: digest}
	for _, key := range []string{app.Fingerprint, assumedKey(app.Image)} {
		if v, ok := d.imageDigests[key]; ok {
			keep[key] = v
		}
	}
	d.imageDigests = keep
	return true
}

// setImageDigest sets one entry of d.imageDigests. The map is replaced, not
// changed, because saveState marshals it without holding a.mu. Callers hold
// a.mu.
func (d *Device) setImageDigest(key, digest string) {
	m := maps.Clone(d.imageDigests)
	if m == nil {
		m = map[string]string{}
	}
	m[key] = digest
	d.imageDigests = m
}

// composeDir finds the directory of an incus-compose project. The projects
// are cached for composeCacheTTL, so the directories are read once for all
// apps of a check cycle rather than once per app.
func (a *Agent) composeDir(project string) (string, bool) {
	a.composeMu.Lock()
	defer a.composeMu.Unlock()
	if a.composeProjects == nil || a.now().Sub(a.composeTime) >= composeCacheTTL {
		a.composeProjects = incuscompose.Find(a.cfg.Incus.ComposeDirs)
		a.composeTime = a.now()
	}
	dir, ok := a.composeProjects[project]
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
		// `incus-compose up` would start a stopped service.
		if !d.running() {
			return false
		}
		_, ok := a.composeDir(app.ComposeProject)
		return ok
	default:
		_, ok := d.Provider.(guest.AppProvider)
		return ok
	}
}

// upgradeApp updates an application container to the newest version of its
// image: through incus-compose when it manages the container, otherwise by
// rebuilding the instance from the registry. A rebuild stops the container,
// so it is skipped when the registry has the installed image; when the
// registry cannot be asked, only an upgrade of this one device (not one of
// all devices) rebuilds anyway.
func (a *Agent) upgradeApp(ctx context.Context, d *Device, report *RunReport, all bool) {
	a.mu.Lock()
	g := *d.Guest
	a.mu.Unlock()
	app := g.App
	if app == nil {
		report.step("image", false, errors.New("the instance is no application container"), "")
		return
	}
	if app.Image == "" {
		report.step("image", false, errors.New(noImage(*app)), "")
		return
	}

	if app.ComposeProject != "" {
		if !g.Running() {
			report.step("incus-compose", false, errors.New("the service is not running, and incus-compose would start it"), "")
			return
		}
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
		report.step("image", false, fmt.Errorf("%s cannot update application containers; recreate it from the newer image", g.Platform), "")
		return
	}
	latest, err := a.latestFingerprint(ctx, app)
	switch {
	case err == nil && latest == app.Fingerprint:
		report.step("image", false, nil, "up to date")
		return
	case err != nil && all:
		report.step("image", false, nil, "skipped: cannot tell whether a newer image exists: "+err.Error())
		return
	}
	// Incus cannot rebuild an instance that has snapshots, so none is taken
	// (the data of application containers belongs in volumes, which a
	// rebuild keeps), and the user's own snapshots are left alone.
	snaps, err := d.Provider.Snapshots(ctx, g)
	switch {
	case err != nil:
		report.step("rebuild from "+app.Image, false, err, "")
		return
	case len(snaps) > 0:
		report.step("rebuild from "+app.Image, false, fmt.Errorf("the instance has %d snapshot(s), and Incus cannot rebuild an instance with snapshots; remove them to update it", len(snaps)), "")
		return
	}
	err = rebuilder.RebuildApp(ctx, g, a.cfg.Guests.StopTimeout)
	changed := a.refreshGuest(ctx, d, app.Fingerprint)
	report.step("rebuild from "+app.Image, changed, err, imageDetail(changed, app.Image))
}

// latestFingerprint asks the registry for the fingerprint of the newest
// version of an app's image.
func (a *Agent) latestFingerprint(ctx context.Context, app *guest.App) (string, error) {
	resolver, ok := a.registry.(containers.FingerprintResolver)
	if !ok {
		return "", errors.New("no registry to ask")
	}
	return resolver.Fingerprint(ctx, app.Image, app.Architecture)
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
