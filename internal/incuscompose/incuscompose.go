// Package incuscompose updates application containers that incus-compose
// manages. incus-compose labels its instances with their project and service
// but does not record where the compose file is, so projects are found by
// name in configured directories. Updates must go through incus-compose: it
// recreates instances whose image differs from the one it last pulled, so an
// update made behind its back would be undone by the next `incus-compose up`.
package incuscompose

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// Files are the compose file names looked for, in order.
var Files = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// Find returns the directory of every compose project by project name: each
// of dirs and their direct subdirectories that contain a compose file.
func Find(dirs []string) map[string]string {
	projects := map[string]string{}
	add := func(dir string) {
		for _, f := range Files {
			data, err := os.ReadFile(filepath.Join(dir, f)) //nolint:gosec // configured directories
			if err != nil {
				continue
			}
			name := ProjectName(dir, data)
			if _, seen := projects[name]; !seen && name != "" {
				projects[name] = dir
			}
			return
		}
	}
	for _, dir := range dirs {
		add(dir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				add(filepath.Join(dir, e.Name()))
			}
		}
	}
	return projects
}

var invalid = regexp.MustCompile(`[^a-z0-9_-]+`)

// ProjectName is the compose project name: the top-level name in the file,
// or else the directory name, normalized like the compose specification.
func ProjectName(dir string, compose []byte) string {
	var top struct {
		Name string `yaml:"name"`
	}
	name := filepath.Base(dir)
	if yaml.Unmarshal(compose, &top) == nil && top.Name != "" && !strings.Contains(top.Name, "${") {
		name = top.Name
	}
	return strings.TrimLeft(invalid.ReplaceAllString(strings.ToLower(name), ""), "_-")
}

// ServiceImages returns the image of every service in a project directory,
// as written in its compose file. incus-compose launches instances from the
// digest it resolved, so the tag is only found here. Images with variables
// are left out: their value depends on an environment the agent does not
// have.
func ServiceImages(dir string) map[string]string {
	images := map[string]string{}
	for _, f := range Files {
		data, err := os.ReadFile(filepath.Join(dir, f)) //nolint:gosec // configured directories
		if err != nil {
			continue
		}
		var file struct {
			Services map[string]struct {
				Image string `yaml:"image"`
			} `yaml:"services"`
		}
		if yaml.Unmarshal(data, &file) != nil {
			return images
		}
		for name, s := range file.Services {
			if s.Image != "" && !strings.Contains(s.Image, "$") {
				images[name] = s.Image
			}
		}
		return images
	}
	return images
}

// Upgrade pulls the newest image of one service and recreates its instance
// when the image changed (`incus-compose up --pull always`), in the project
// directory on the host. The agent's INCUS_* variables are passed on, so
// incus-compose settings such as INCUS_COMPOSE_NO_HEALTHD can be set on the
// service; commands otherwise get a minimal environment.
func Upgrade(ctx context.Context, r runner.Runner, dir, service string) error {
	if !runner.Has(ctx, r, "incus-compose") {
		return errors.New("incus-compose is not installed")
	}
	cmd := runner.Command("sh", "-c", `cd "$1" && shift && exec "$@"`, "sh", dir,
		"incus-compose", "up", "--detach", "--pull", "always", "--no-deps", service)
	_, err := runner.Check(ctx, r, cmd.WithEnv(incusEnv()...))
	return err
}

// incusEnv returns the agent's INCUS_* variables as key, value pairs.
func incusEnv() []string {
	var kv []string
	for _, e := range os.Environ() {
		if k, v, ok := strings.Cut(e, "="); ok && strings.HasPrefix(k, "INCUS_") {
			kv = append(kv, k, v)
		}
	}
	return kv
}
