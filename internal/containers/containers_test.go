package containers

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/lvlie/incus-ha-agent/internal/runner"
)

const dockerInspect = `[
 {"Id":"aaa","Name":"/app-web-1","Image":"sha256:img1",
  "Config":{"Image":"nginx:1.27","Labels":{
    "com.docker.compose.project":"app",
    "com.docker.compose.project.working_dir":"/root/app",
    "com.docker.compose.project.config_files":"/root/app/compose.yaml,/root/app/compose.override.yaml"}},
  "State":{"Status":"running"}},
 {"Id":"bbb","Name":"/app-db-1","Image":"sha256:img2",
  "Config":{"Image":"postgres:17","Labels":{"com.docker.compose.project":"app",
    "com.docker.compose.project.working_dir":"/root/app"}},
  "State":{"Status":"exited"}},
 {"Id":"ccc","Name":"/standalone","Image":"sha256:img3","Config":{"Image":"redis:7","Labels":null},"State":{"Status":"running"}}
]`

// Podman uses bare image IDs and its own label for auto-updates.
const podmanInspect = `[
 {"Id":"ddd","Name":"jellyfin","Image":"img4",
  "Config":{"Image":"docker.io/jellyfin/jellyfin:latest","Labels":{"io.containers.autoupdate":"registry"}},
  "State":{"Status":"running"}}
]`

func TestParseInspect(t *testing.T) {
	cs, err := ParseInspect(dockerInspect)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 3 || cs[0].Name != "app-web-1" || cs[0].ImageID != "img1" || cs[0].Project != "app" ||
		len(cs[0].ConfigFiles) != 2 || !cs[0].Running() || cs[1].Running() {
		t.Fatalf("unexpected %+v", cs)
	}
	ps, err := ParseInspect(podmanInspect)
	if err != nil || !ps[0].AutoUpdate || ps[0].ImageID != "img4" || ps[0].Name != "jellyfin" {
		t.Fatalf("unexpected %+v %v", ps, err)
	}
	if _, err := ParseInspect("not json"); err == nil {
		t.Fatal("expected error")
	}
}

func TestProjects(t *testing.T) {
	cs, _ := ParseInspect(dockerInspect)
	ps := Projects(cs)
	if len(ps) != 1 || ps[0].Name != "app" || ps[0].Containers != 2 || ps[0].Running != 1 || ps[0].WorkingDir != "/root/app" {
		t.Fatalf("unexpected %+v", ps)
	}
}

func TestParseImageInspect(t *testing.T) {
	got, err := ParseImageInspect(`[{"Id":"sha256:img1","RepoDigests":["nginx@sha256:old","docker.io/library/nginx@sha256:old2"]},{"Id":"img9","RepoDigests":[]}]`)
	if err != nil || len(got["img1"]) != 2 || got["img1"][0] != "sha256:old" || len(got["img9"]) != 0 {
		t.Fatalf("unexpected %v %v", got, err)
	}
}

type fakeResolver map[string]string

func (f fakeResolver) Digest(_ context.Context, ref string) (string, error) {
	if d, ok := f[ref]; ok {
		return d, nil
	}
	return "", errors.New("unauthorized")
}

func TestCheckImages(t *testing.T) {
	cs, _ := ParseInspect(dockerInspect)
	cs = append(cs, Container{Name: "pinned", Image: "busybox@sha256:abc", ImageID: "img5"})
	local := map[string][]string{"img1": {"sha256:old"}, "img2": {"sha256:pg"}, "img3": {"sha256:r1"}}
	check := CheckImages(context.Background(), cs, local, fakeResolver{
		"nginx:1.27":  "sha256:new",
		"postgres:17": "sha256:pg",
		"redis:7":     "sha256:r2",
	})
	if check.Unknown != 0 || len(check.Updates) != 2 {
		t.Fatalf("unexpected %+v", check)
	}
	if check.Updates[0].Image != "nginx:1.27" || !check.Updates[0].Managed {
		t.Errorf("nginx: %+v", check.Updates[0])
	}
	if check.Updates[1].Image != "redis:7" || check.Updates[1].Managed {
		t.Errorf("standalone redis must not be managed: %+v", check.Updates[1])
	}
	unknown := CheckImages(context.Background(), cs[:1], map[string][]string{}, fakeResolver{})
	if unknown.Unknown != 1 {
		t.Errorf("local build should be unknown: %+v", unknown)
	}
}

func TestRegistryDigest(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := name.ParseReference(host + "/test/app:1")
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	want, _ := img.Digest()
	r := &Registry{}
	got, err := r.Digest(context.Background(), host+"/test/app:1")
	if err != nil || got != want.String() {
		t.Fatalf("got %q %v, want %q", got, err, want)
	}
	if _, err := r.Digest(context.Background(), host+"/test/missing:1"); err == nil {
		t.Fatal("expected error for missing image")
	}
	srv.Close()
	// Cached: still answers without the registry.
	if again, err := r.Digest(context.Background(), host+"/test/app:1"); err != nil || again != got {
		t.Fatalf("cache miss: %q %v", again, err)
	}
	r.Reset()
	if _, err := r.Digest(context.Background(), host+"/test/app:1"); err == nil {
		t.Fatal("expected error after reset with registry down")
	}
}

func TestDetect(t *testing.T) {
	ok := runner.Result{}
	both := (&runner.Fake{}).
		On("command -v \"$1\" >/dev/null 2>&1 sh docker", ok).
		On("command -v \"$1\" >/dev/null 2>&1 sh podman", ok).
		On("docker version", runner.Result{Stdout: "27.5.1\n"}).
		On("docker --version", runner.Result{Stdout: "Docker version 27.5.1, build 9f9e405\n"})
	if es := Detect(context.Background(), both); len(es) != 2 || es[0].Name != "docker" || es[1].Name != "podman" {
		t.Fatalf("unexpected %+v", es)
	}
	shim := (&runner.Fake{}).
		On("command -v \"$1\" >/dev/null 2>&1 sh docker", ok).
		On("command -v \"$1\" >/dev/null 2>&1 sh podman", ok).
		On("docker version", runner.Result{Stdout: "5.6.1\n"}).
		On("docker --version", runner.Result{Stdout: "podman version 5.6.1\n"})
	if es := Detect(context.Background(), shim); len(es) != 1 || es[0].Name != "podman" {
		t.Fatalf("podman-docker shim must only count as podman: %+v", es)
	}
	none := &runner.Fake{}
	if es := Detect(context.Background(), none); len(es) != 0 {
		t.Fatalf("unexpected %+v", es)
	}
	noDaemon := (&runner.Fake{}).
		On("command -v \"$1\" >/dev/null 2>&1 sh docker", ok).
		On("docker version", runner.Result{ExitCode: 1, Stderr: "Cannot connect to the Docker daemon"}).
		On("docker --version", runner.Result{Stdout: "Docker version 27.5.1\n"})
	if es := Detect(context.Background(), noDaemon); len(es) != 0 {
		t.Fatalf("docker without daemon must be skipped: %+v", es)
	}
}

func TestUpgradeProject(t *testing.T) {
	imageID := "sha256:img1"
	inspect := func(runner.Cmd) runner.Result {
		return runner.Result{Stdout: fmt.Sprintf(`[{"Id":"aaa","Name":"/app-web-1","Image":%q,
		"Config":{"Image":"nginx:1.27","Labels":{"com.docker.compose.project":"app"}},"State":{"Status":"running"}}]`, imageID)}
	}
	f := (&runner.Fake{}).
		On("docker ps -a -q --no-trunc", runner.Result{Stdout: "aaa\n"}).
		OnFunc("docker container inspect aaa", inspect).
		On("pull --quiet", runner.Result{}).
		OnFunc("up -d --remove-orphans", func(runner.Cmd) runner.Result { imageID = "sha256:img2"; return runner.Result{} })
	e := NewEngine("docker", f)
	p := Project{Name: "app", WorkingDir: "/root/app", ConfigFiles: []string{"/root/app/compose.yaml"}}
	changed, err := e.UpgradeProject(context.Background(), p)
	if err != nil || !changed {
		t.Fatalf("expected change, got %v %v", changed, err)
	}
	want := "docker compose -p app --project-directory /root/app -f /root/app/compose.yaml pull --quiet"
	if !f.Ran(want) {
		t.Fatalf("missing %q in %v", want, f.Calls)
	}
	changed, err = e.UpgradeProject(context.Background(), p)
	if err != nil || changed {
		t.Fatalf("second upgrade must not change, got %v %v", changed, err)
	}
}

func TestPodmanComposeFallback(t *testing.T) {
	f := (&runner.Fake{}).
		On("podman compose version", runner.Result{ExitCode: 125}).
		On("command -v \"$1\" >/dev/null 2>&1 sh podman-compose", runner.Result{})
	if got := NewEngine("podman", f).compose(context.Background()); strings.Join(got, " ") != "podman-compose" {
		t.Fatalf("got %v", got)
	}
}

func TestPruneAndAutoUpdate(t *testing.T) {
	d := NewEngine("docker", (&runner.Fake{}).On("image prune", runner.Result{Stdout: "Total reclaimed space: 0B\n"}))
	if changed, err := d.PruneImages(context.Background()); err != nil || changed {
		t.Fatalf("docker prune: %v %v", changed, err)
	}
	p := NewEngine("podman", (&runner.Fake{}).
		On("image prune", runner.Result{Stdout: "3f2a1b\n"}).
		On("auto-update", runner.Result{Stdout: "jellyfin.service|true\nother.service|false\n"}))
	if changed, err := p.PruneImages(context.Background()); err != nil || !changed {
		t.Fatalf("podman prune: %v %v", changed, err)
	}
	if changed, err := p.AutoUpdate(context.Background()); err != nil || !changed {
		t.Fatalf("auto-update: %v %v", changed, err)
	}
	if changed, _ := d.AutoUpdate(context.Background()); changed {
		t.Fatal("docker has no auto-update")
	}
}
