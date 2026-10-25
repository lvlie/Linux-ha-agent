package incus

import (
	"strings"
	"testing"
)

func TestApp(t *testing.T) {
	if App(map[string]string{"image.os": "Ubuntu"}) != nil {
		t.Error("system container detected as app")
	}
	cases := map[string]struct {
		config map[string]string
		image  string
	}{
		"tag":     {map[string]string{"image.id": "nginx:1.27"}, "docker.io/library/nginx:1.27"},
		"no tag":  {map[string]string{"image.id": "nginx"}, "docker.io/library/nginx:latest"},
		"pinned":  {map[string]string{"image.id": "nginx:1.27@sha256:abc"}, "docker.io/library/nginx@sha256:abc"},
		"port":    {map[string]string{"image.id": "localhost:5000/nginx"}, "docker.io/library/nginx:latest"},
		"flagged": {map[string]string{"volatile.container.oci": "true", "image.type": "", "image.id": "nginx:1"}, "docker.io/library/nginx:1"},
	}
	for name, c := range cases {
		cfg := map[string]string{"image.type": "oci", "image.description": "docker.io/library/nginx (OCI)",
			"image.architecture": "amd64", "volatile.base_image": "fp"}
		for k, v := range c.config {
			cfg[k] = v
		}
		app := App(cfg)
		if app == nil || app.Image != c.image || app.Fingerprint != "fp" || app.Architecture != "amd64" {
			t.Errorf("%s: %+v", name, app)
		}
		if pinned := strings.Contains(c.image, "@"); pinned != (app.Digest == "sha256:abc") || !pinned && app.Digest != "" {
			t.Errorf("%s: digest %q", name, app.Digest)
		}
	}
	app := App(map[string]string{"image.type": "oci", "image.description": "ghcr.io/me/app (OCI)", "image.id": "ghcr.io/me/app:v2",
		"user.label.incus-compose.project": "web", "user.label.incus-compose.service": "app"})
	if app.Image != "ghcr.io/me/app:v2" || app.ComposeProject != "web" || app.ComposeService != "app" {
		t.Errorf("compose: %+v", app)
	}
	if app := App(map[string]string{"image.type": "oci"}); app == nil || app.Image != "" {
		t.Errorf("without image info: %+v", app)
	}
}

func TestOCIArchitecture(t *testing.T) {
	for incus, oci := range map[string]string{"x86_64": "amd64", "aarch64": "arm64", "armv7l": "arm/v7", "armv6l": "arm/v6", "amd64": "amd64", "": ""} {
		if got := App(map[string]string{"image.type": "oci", "image.architecture": incus}).Architecture; got != oci {
			t.Errorf("%q: got %q, want %q", incus, got, oci)
		}
	}
}
