// Package sysinfo collects system information from a target in one command:
// kernel versions, uptime, load, CPU, memory, disk and temperature.
package sysinfo

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// Info describes the state of a machine.
type Info struct {
	Kernel       string        `json:"kernel"`
	LatestKernel string        `json:"latest_kernel,omitempty"`
	Arch         string        `json:"arch"`
	Uptime       time.Duration `json:"uptime"`
	Load1        float64       `json:"load_1"`
	Load5        float64       `json:"load_5"`
	Load15       float64       `json:"load_15"`
	CPUs         int           `json:"cpus"`
	// CPUPercent is nil when it could not be measured.
	CPUPercent   *float64 `json:"cpu_percent"`
	MemTotal     uint64   `json:"mem_total"`
	MemAvailable uint64   `json:"mem_available"`
	DiskTotal    uint64   `json:"disk_total"`
	DiskUsed     uint64   `json:"disk_used"`
	// TemperatureC is the hottest thermal zone; nil when there is none.
	TemperatureC *float64 `json:"temperature"`
}

// script prints key=value lines. It only uses POSIX sh, coreutils/busybox
// and awk, so it runs on every supported distribution and on Alpine.
const script = `echo "kernel=$(uname -r)"
echo "arch=$(uname -m)"
echo "latest_kernel=$(ls /boot/vmlinuz-* 2>/dev/null | sed 's|^.*/vmlinuz-||' | { sort -V 2>/dev/null || sort; } | tail -n 1)"
echo "uptime=$(cut -d' ' -f1 /proc/uptime)"
echo "load=$(cut -d' ' -f1-3 /proc/loadavg)"
echo "cpus=$(nproc 2>/dev/null || grep -c ^processor /proc/cpuinfo)"
awk '/^MemTotal:/{print "mem_total="$2} /^MemAvailable:/{print "mem_available="$2}' /proc/meminfo
df -Pk / | awk 'NR==2{print "disk_total="$2; print "disk_used="$3}'
cpu() { awk '/^cpu /{print $2+$3+$4+$5+$6+$7+$8+$9, $5+$6}' /proc/stat; }
a=$(cpu); sleep 1; b=$(cpu)
echo "cpu=$a $b"
echo "temp=$(cat /sys/class/thermal/thermal_zone*/temp 2>/dev/null | sort -n | tail -n 1)"
`

// Collect gathers the information. Containers share the host's kernel and
// sensors, so for them the installed kernel and the temperature are skipped.
func Collect(ctx context.Context, r runner.Runner, container bool) (Info, error) {
	res, err := runner.Check(ctx, r, runner.Command("sh", "-c", script))
	if err != nil {
		return Info{}, err
	}
	info := Parse(res.Stdout)
	if container {
		info.LatestKernel = ""
		info.TemperatureC = nil
	}
	return info, nil
}

// Parse parses the script output.
func Parse(out string) Info {
	var info Info
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "kernel":
			info.Kernel = value
		case "latest_kernel":
			info.LatestKernel = value
		case "arch":
			info.Arch = value
		case "uptime":
			first, _, _ := strings.Cut(value, " ")
			if secs, err := strconv.ParseFloat(first, 64); err == nil {
				info.Uptime = time.Duration(secs * float64(time.Second))
			}
		case "load":
			f := strings.Fields(value)
			if len(f) == 3 {
				info.Load1, _ = strconv.ParseFloat(f[0], 64)
				info.Load5, _ = strconv.ParseFloat(f[1], 64)
				info.Load15, _ = strconv.ParseFloat(f[2], 64)
			}
		case "cpus":
			info.CPUs, _ = strconv.Atoi(value)
		case "mem_total":
			info.MemTotal = kib(value)
		case "mem_available":
			info.MemAvailable = kib(value)
		case "disk_total":
			info.DiskTotal = kib(value)
		case "disk_used":
			info.DiskUsed = kib(value)
		case "cpu":
			info.CPUPercent = cpuPercent(strings.Fields(value))
		case "temp":
			if milli, err := strconv.ParseFloat(value, 64); err == nil && milli > 0 {
				c := round1(milli / 1000)
				info.TemperatureC = &c
			}
		}
	}
	return info
}

func kib(s string) uint64 {
	n, _ := strconv.ParseUint(s, 10, 64)
	return n * 1024
}

// cpuPercent computes the busy share between two /proc/stat samples of
// "total idle".
func cpuPercent(f []string) *float64 {
	if len(f) != 4 {
		return nil
	}
	var v [4]float64
	for i, s := range f {
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil
		}
		v[i] = n
	}
	total, idle := v[2]-v[0], v[3]-v[1]
	if total <= 0 {
		return nil
	}
	p := round1(100 * (total - idle) / total)
	return &p
}

func round1(f float64) float64 {
	return float64(int64(f*10+0.5)) / 10
}

// KernelUpdatePending reports whether a newer kernel is installed than the
// one running.
func (i Info) KernelUpdatePending() bool {
	return i.LatestKernel != "" && i.Kernel != "" && i.LatestKernel != i.Kernel
}

// MemUsedPercent is the share of memory in use.
func (i Info) MemUsedPercent() *float64 {
	if i.MemTotal == 0 {
		return nil
	}
	p := round1(100 * float64(i.MemTotal-min(i.MemAvailable, i.MemTotal)) / float64(i.MemTotal))
	return &p
}

// DiskUsedPercent is the share of the root file system in use.
func (i Info) DiskUsedPercent() *float64 {
	if i.DiskTotal == 0 {
		return nil
	}
	p := round1(100 * float64(i.DiskUsed) / float64(i.DiskTotal))
	return &p
}

// BootTime is when the machine started, rounded to the minute so it does not
// change on every refresh.
func (i Info) BootTime(now time.Time) time.Time {
	return now.Add(-i.Uptime).Truncate(time.Minute)
}
