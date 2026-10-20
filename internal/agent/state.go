package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/lvlie/linux-ha-agent/internal/containers"
	"github.com/lvlie/linux-ha-agent/internal/packages"
)

// Step is one step of an upgrade.
type Step struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Changed bool   `json:"changed"`
	Detail  string `json:"detail,omitempty"`
}

// RunReport describes one upgrade of a device.
type RunReport struct {
	Trigger  string    `json:"trigger"`
	Action   string    `json:"action"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Success  bool      `json:"success"`
	Changed  bool      `json:"changed"`
	Steps    []Step    `json:"steps"`
}

func (r *RunReport) step(name string, changed bool, err error, detail string) bool {
	s := Step{Name: name, OK: err == nil, Changed: changed, Detail: detail}
	if err != nil {
		s.Detail = err.Error()
	}
	r.Steps = append(r.Steps, s)
	r.Changed = r.Changed || changed
	return err == nil
}

// Result is a one-word summary for Home Assistant.
func (r *RunReport) Result() string {
	if r == nil {
		return "never"
	}
	if !r.Success {
		return "failed"
	}
	if r.Changed {
		return "upgraded"
	}
	return "up to date"
}

// Check is the latest inspection of a device.
type Check struct {
	Time           time.Time
	Error          string
	OS             string
	Packages       *packages.Status
	Engines        []string
	Containers     []containers.Container
	Projects       []containers.Project
	Images         containers.ImageCheck
	ImagesChecked  bool
	RebootRequired bool
}

// persisted is the agent's state file: things that must survive restarts.
type persisted struct {
	AutoUpgrade *bool                 `json:"auto_upgrade,omitempty"`
	LastRuns    map[string]*RunReport `json:"last_runs"`
	Devices     []string              `json:"devices"`
}

func loadState(path string) (persisted, error) {
	st := persisted{LastRuns: map[string]*RunReport{}}
	data, err := os.ReadFile(path) //nolint:gosec // path from config
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("reading state: %w", err)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return persisted{LastRuns: map[string]*RunReport{}}, fmt.Errorf("parsing state %s: %w", path, err)
	}
	if st.LastRuns == nil {
		st.LastRuns = map[string]*RunReport{}
	}
	return st, nil
}

// saveState writes atomically so a crash never leaves a broken file.
func saveState(path string, st persisted) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
