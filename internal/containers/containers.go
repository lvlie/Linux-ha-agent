// Package containers inspects and updates Docker and Podman workloads on a
// target. Compose projects are found from the labels compose puts on their
// containers, so they are picked up wherever their files live.
package containers

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// Compose labels set by docker compose and podman-compose.
const (
	labelProject     = "com.docker.compose.project"
	labelWorkingDir  = "com.docker.compose.project.working_dir"
	labelConfigFiles = "com.docker.compose.project.config_files"
	labelService     = "com.docker.compose.service"
	labelAutoUpdate  = "io.containers.autoupdate"
)

// Engine is a container engine (docker or podman) on one target.
type Engine struct {
	Name string
	r    runner.Runner
}

// Detect returns the engines usable on the target. A docker command that is
// really podman (podman-docker) is not reported twice.
func Detect(ctx context.Context, r runner.Runner) []*Engine {
	var engines []*Engine
	if runner.Has(ctx, r, "docker") {
		res, err := r.Run(ctx, runner.Command("docker", "version", "--format", "{{.Server.Version}}"))
		version, _ := r.Run(ctx, runner.Command("docker", "--version"))
		isPodman := strings.Contains(strings.ToLower(version.Stdout), "podman")
		if err == nil && res.ExitCode == 0 && !isPodman {
			engines = append(engines, &Engine{Name: "docker", r: r})
		}
	}
	if runner.Has(ctx, r, "podman") {
		engines = append(engines, &Engine{Name: "podman", r: r})
	}
	return engines
}

// NewEngine returns an engine without detection (for tests).
func NewEngine(name string, r runner.Runner) *Engine {
	return &Engine{Name: name, r: r}
}

// Container is a container with the details the agent needs.
type Container struct {
	ID          string
	Name        string
	Image       string
	ImageID     string
	State       string
	Project     string
	Service     string
	WorkingDir  string
	ConfigFiles []string
	AutoUpdate  bool
}

// Running reports whether the container runs.
func (c Container) Running() bool {
	return c.State == "running"
}

type inspectContainer struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Image  string `json:"Image"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Status string `json:"Status"`
	} `json:"State"`
}

// Containers lists all containers, running or not.
func (e *Engine) Containers(ctx context.Context) ([]Container, error) {
	res, err := runner.Check(ctx, e.r, runner.Command(e.Name, "ps", "-a", "-q", "--no-trunc"))
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(res.Stdout)
	if len(ids) == 0 {
		return nil, nil
	}
	res, err = runner.Check(ctx, e.r, runner.Command(append([]string{e.Name, "container", "inspect"}, ids...)...))
	if err != nil {
		return nil, err
	}
	return ParseInspect(res.Stdout)
}

// ParseInspect parses `docker/podman container inspect` output.
func ParseInspect(out string) ([]Container, error) {
	var raw []inspectContainer
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parsing container inspect output: %w", err)
	}
	containers := make([]Container, 0, len(raw))
	for _, c := range raw {
		labels := c.Config.Labels
		var files []string
		for _, f := range strings.Split(labels[labelConfigFiles], ",") {
			if f = strings.TrimSpace(f); f != "" {
				files = append(files, f)
			}
		}
		autoUpdate := labels[labelAutoUpdate]
		containers = append(containers, Container{
			ID:          c.ID,
			Name:        strings.TrimPrefix(c.Name, "/"),
			Image:       c.Config.Image,
			ImageID:     normalizeID(c.Image),
			State:       strings.ToLower(c.State.Status),
			Project:     labels[labelProject],
			Service:     labels[labelService],
			WorkingDir:  labels[labelWorkingDir],
			ConfigFiles: files,
			AutoUpdate:  autoUpdate == "registry" || autoUpdate == "local",
		})
	}
	return containers, nil
}

func normalizeID(id string) string {
	return strings.TrimPrefix(id, "sha256:")
}

// Project is a compose project.
type Project struct {
	Name        string   `json:"name"`
	WorkingDir  string   `json:"working_dir"`
	ConfigFiles []string `json:"config_files,omitempty"`
	Containers  int      `json:"containers"`
	Running     int      `json:"running"`
}

// Projects groups containers into compose projects.
func Projects(containers []Container) []Project {
	byName := map[string]*Project{}
	for _, c := range containers {
		if c.Project == "" {
			continue
		}
		p := byName[c.Project]
		if p == nil {
			p = &Project{Name: c.Project, WorkingDir: c.WorkingDir, ConfigFiles: c.ConfigFiles}
			byName[c.Project] = p
		}
		p.Containers++
		if c.Running() {
			p.Running++
		}
	}
	projects := make([]Project, 0, len(byName))
	for _, p := range byName {
		projects = append(projects, *p)
	}
	slices.SortFunc(projects, func(a, b Project) int { return strings.Compare(a.Name, b.Name) })
	return projects
}

// RepoDigests returns the registry digests of each image ID.
func (e *Engine) RepoDigests(ctx context.Context, imageIDs []string) (map[string][]string, error) {
	if len(imageIDs) == 0 {
		return map[string][]string{}, nil
	}
	res, err := runner.Check(ctx, e.r, runner.Command(append([]string{e.Name, "image", "inspect"}, imageIDs...)...))
	if err != nil {
		return nil, err
	}
	return ParseImageInspect(res.Stdout)
}

// ParseImageInspect maps image IDs to the digest part of their RepoDigests.
func ParseImageInspect(out string) (map[string][]string, error) {
	var raw []struct {
		ID          string   `json:"Id"`
		RepoDigests []string `json:"RepoDigests"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parsing image inspect output: %w", err)
	}
	digests := map[string][]string{}
	for _, img := range raw {
		for _, rd := range img.RepoDigests {
			if _, digest, ok := strings.Cut(rd, "@"); ok {
				digests[normalizeID(img.ID)] = append(digests[normalizeID(img.ID)], digest)
			}
		}
	}
	return digests, nil
}

// composeTool is the compose implementation of an engine.
type composeTool struct {
	base []string
	// podmanCompose is the Python podman-compose, which has fewer options.
	podmanCompose bool
}

func (e *Engine) compose(ctx context.Context) composeTool {
	if e.Name == "podman" {
		res, err := e.r.Run(ctx, runner.Command("podman", "compose", "version"))
		if (err != nil || res.ExitCode != 0) && runner.Has(ctx, e.r, "podman-compose") {
			return composeTool{base: []string{"podman-compose"}, podmanCompose: true}
		}
		// `podman compose` may itself delegate to podman-compose.
		return composeTool{base: []string{"podman", "compose"}, podmanCompose: strings.Contains(res.Stdout+res.Stderr, "podman-compose")}
	}
	return composeTool{base: []string{e.Name, "compose"}}
}

// command runs compose from the project's directory, so relative file names
// in the compose labels (podman-compose stores them that way) resolve.
func (c composeTool) command(p Project, args ...string) runner.Cmd {
	cmd := append([]string(nil), c.base...)
	cmd = append(cmd, "-p", p.Name)
	for _, f := range p.ConfigFiles {
		cmd = append(cmd, "-f", f)
	}
	cmd = append(cmd, args...)
	if p.WorkingDir == "" {
		return runner.Command(cmd...)
	}
	return runner.Command(append([]string{"sh", "-c", `cd "$1" && shift && exec "$@"`, "sh", p.WorkingDir}, cmd...)...)
}

func (c composeTool) pull(p Project, services ...string) runner.Cmd {
	args := []string{"pull"}
	if !c.podmanCompose {
		args = append(args, "--quiet")
	}
	return c.command(p, append(args, services...)...)
}

func (c composeTool) up(p Project, services ...string) runner.Cmd {
	args := []string{"up", "-d"}
	if len(services) == 0 {
		args = append(args, "--remove-orphans")
	} else {
		args = append(args, "--no-deps")
	}
	return c.command(p, append(args, services...)...)
}

// UpgradeProject pulls newer images for a compose project and recreates the
// containers whose image changed. It reports whether anything changed.
func (e *Engine) UpgradeProject(ctx context.Context, p Project) (bool, error) {
	return e.upgradeCompose(ctx, p)
}

// UpgradeService does the same for one service of a project.
func (e *Engine) UpgradeService(ctx context.Context, p Project, service string) (bool, error) {
	return e.upgradeCompose(ctx, p, service)
}

func (e *Engine) upgradeCompose(ctx context.Context, p Project, services ...string) (bool, error) {
	before, err := e.projectImages(ctx, p.Name)
	if err != nil {
		return false, err
	}
	tool := e.compose(ctx)
	if _, err := runner.Check(ctx, e.r, tool.pull(p, services...)); err != nil {
		return false, err
	}
	if _, err := runner.Check(ctx, e.r, tool.up(p, services...)); err != nil {
		return false, err
	}
	after, err := e.projectImages(ctx, p.Name)
	if err != nil {
		return false, err
	}
	return !maps.Equal(before, after), nil
}

// Restart restarts one container.
func (e *Engine) Restart(ctx context.Context, id string) error {
	_, err := runner.Check(ctx, e.r, runner.Command(e.Name, "restart", id))
	return err
}

func (e *Engine) projectImages(ctx context.Context, project string) (map[string]string, error) {
	all, err := e.Containers(ctx)
	if err != nil {
		return nil, err
	}
	images := map[string]string{}
	for _, c := range all {
		if c.Project == project {
			images[c.Name] = c.ImageID
		}
	}
	return images, nil
}

// AutoUpdate runs `podman auto-update` for containers labelled for it.
func (e *Engine) AutoUpdate(ctx context.Context) (bool, error) {
	if e.Name != "podman" {
		return false, nil
	}
	res, err := runner.Check(ctx, e.r, runner.Command("podman", "auto-update", "--format", "{{.Unit}}|{{.Updated}}"))
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), "|true") {
			return true, nil
		}
	}
	return false, nil
}

// PruneImages removes dangling images and reports whether any were removed.
func (e *Engine) PruneImages(ctx context.Context) (bool, error) {
	res, err := runner.Check(ctx, e.r, runner.Command(e.Name, "image", "prune", "--force"))
	if err != nil {
		return false, err
	}
	out := strings.TrimSpace(res.Stdout)
	if e.Name == "docker" {
		return out != "" && !strings.Contains(out, "Total reclaimed space: 0B"), nil
	}
	return out != "", nil
}
