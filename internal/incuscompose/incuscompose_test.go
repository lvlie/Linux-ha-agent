package incuscompose

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lvlie/linux-ha-agent/internal/runner"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFind(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "Web App", "compose.yaml"), "services: {}\n")
	write(t, filepath.Join(root, "named", "docker-compose.yml"), "name: media\nservices: {}\n")
	write(t, filepath.Join(root, "templated", "compose.yaml"), "name: ${PROJECT}\n")
	write(t, filepath.Join(root, ".hidden", "compose.yaml"), "services: {}\n")
	write(t, filepath.Join(root, "empty", "README"), "")
	write(t, filepath.Join(root, "compose.yml"), "name: top\n")
	got := Find([]string{root, filepath.Join(root, "missing")})
	want := map[string]string{
		"webapp":    filepath.Join(root, "Web App"),
		"media":     filepath.Join(root, "named"),
		"templated": filepath.Join(root, "templated"),
		"top":       root,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for name, dir := range want {
		if got[name] != dir {
			t.Errorf("%s: %q, want %q", name, got[name], dir)
		}
	}
}

func TestServiceImages(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "compose.yaml"), `name: media
services:
  sonarr:
    image: ghcr.io/linuxserver/sonarr:latest
  db:
    image: "docker.io/library/postgres:16-alpine"
  templated:
    image: ghcr.io/me/app:${TAG:-latest}
  built:
    build: .
`)
	write(t, filepath.Join(dir, "docker-compose.yml"), "services: {sonarr: {image: ignored}}\n")
	got := ServiceImages(dir)
	want := map[string]string{"sonarr": "ghcr.io/linuxserver/sonarr:latest", "db": "docker.io/library/postgres:16-alpine"}
	if len(got) != len(want) || got["sonarr"] != want["sonarr"] || got["db"] != want["db"] {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := ServiceImages(filepath.Join(dir, "missing")); len(got) != 0 {
		t.Errorf("missing directory: %v", got)
	}
}

func TestUpgrade(t *testing.T) {
	f := (&runner.Fake{}).On("sh incus-compose", runner.Result{}).On("incus-compose up", runner.Result{})
	if err := Upgrade(context.Background(), f, "/root/web", "app"); err != nil {
		t.Fatal(err)
	}
	last := f.Calls[len(f.Calls)-1].Args
	want := []string{"sh", "-c", `cd "$1" && shift && exec "$@"`, "sh", "/root/web", "incus-compose", "up", "--detach", "--pull", "always", "--no-deps", "app"}
	if len(last) != len(want) {
		t.Fatalf("got %q", last)
	}
	for i := range want {
		if last[i] != want[i] {
			t.Fatalf("got %q", last)
		}
	}
	if err := Upgrade(context.Background(), &runner.Fake{}, "/root/web", "app"); err == nil {
		t.Error("expected an error without incus-compose")
	}
}
