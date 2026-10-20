package sysinfo

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/lvlie/linux-ha-agent/internal/runner"
)

const sample = `kernel=6.8.12-4-pve
arch=x86_64
latest_kernel=6.8.12-9-pve
uptime=93784.21 351234.00
load=0.52 0.48 0.40
cpus=8
mem_total=32768000
mem_available=8192000
disk_total=100000000
disk_used=25000000
cpu=1000 800 1400 1100
temp=54500
`

func TestParse(t *testing.T) {
	i := Parse(sample)
	if i.Kernel != "6.8.12-4-pve" || i.LatestKernel != "6.8.12-9-pve" || !i.KernelUpdatePending() || i.Arch != "x86_64" {
		t.Errorf("kernel: %+v", i)
	}
	if i.Uptime.Round(time.Second) != 93784*time.Second || i.Load1 != 0.52 || i.Load15 != 0.40 || i.CPUs != 8 {
		t.Errorf("uptime/load: %+v", i)
	}
	if *i.CPUPercent != 25 { // 400 total, 300 idle
		t.Errorf("cpu %v", *i.CPUPercent)
	}
	if *i.MemUsedPercent() != 75 || *i.DiskUsedPercent() != 25 || *i.TemperatureC != 54.5 {
		t.Errorf("mem %v disk %v temp %v", *i.MemUsedPercent(), *i.DiskUsedPercent(), *i.TemperatureC)
	}
	now := time.Date(2026, 10, 20, 12, 0, 30, 0, time.UTC)
	if got := i.BootTime(now); !got.Equal(time.Date(2026, 10, 19, 9, 57, 0, 0, time.UTC)) {
		t.Errorf("boot time %v", got)
	}
}

func TestParseMissingValues(t *testing.T) {
	i := Parse("kernel=6.1.0\nlatest_kernel=\ncpu=1 1 1 1\ntemp=\nload=garbage\n")
	if i.KernelUpdatePending() || i.CPUPercent != nil || i.TemperatureC != nil || i.MemUsedPercent() != nil || i.DiskUsedPercent() != nil {
		t.Errorf("unexpected %+v", i)
	}
	same := Parse("kernel=6.8.0-45-generic\nlatest_kernel=6.8.0-45-generic\n")
	if same.KernelUpdatePending() {
		t.Error("running the newest kernel")
	}
}

func TestCollectContainerSkipsKernelAndTemperature(t *testing.T) {
	f := (&runner.Fake{}).On("sh -c", runner.Result{Stdout: sample})
	i, err := Collect(context.Background(), f, true)
	if err != nil || i.LatestKernel != "" || i.TemperatureC != nil || i.KernelUpdatePending() || i.Kernel == "" {
		t.Fatalf("got %+v %v", i, err)
	}
}

// TestScriptRunsHere runs the real script on the test machine.
func TestScriptRunsHere(t *testing.T) {
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("no awk")
	}
	i, err := Collect(context.Background(), runner.Local{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if i.Kernel == "" || i.Arch == "" || i.Uptime <= 0 || i.CPUs <= 0 || i.MemTotal == 0 || i.DiskTotal == 0 || i.CPUPercent == nil {
		t.Fatalf("incomplete: %+v", i)
	}
}
