package commands

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/lvlie/linux-ha-agent/internal/runner"
)

func now() time.Time { return time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC) }

func TestValue(t *testing.T) {
	for out, want := range map[string]string{
		"42\n":                   "42",
		"  first \nsecond\n":     "first",
		"":                       "",
		strings.Repeat("x", 300): strings.Repeat("x", 252) + "...",
	} {
		if got := (Result{Output: out}).Value(); got != want {
			t.Errorf("Value(%q) = %q", out, got)
		}
	}
}

func TestSetupErrorIsReported(t *testing.T) {
	f := (&runner.Fake{}).On("setpriv", runner.Result{ExitCode: ExitSetup, Stderr: "linux-ha-agent: setpriv not found (install util-linux)\n"})
	r := Run(context.Background(), f, "id", time.Second, now)
	if r.Error != "setpriv not found (install util-linux)" || r.ExitCode != ExitSetup {
		t.Fatalf("%+v", r)
	}
	if args := f.Calls[0].Args; args[0] != "sh" || args[4] != "id" || args[5] != "1" || f.Calls[0].Env["HOME"] != "/" {
		t.Fatalf("command: %q %v", args, f.Calls[0].Env)
	}
}

// TestUnprivilegedHere runs the real wrapper; it needs root and setpriv.
func TestUnprivilegedHere(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := exec.LookPath("setpriv"); err != nil {
		t.Skip("no setpriv")
	}
	ctx := context.Background()
	run := func(shell string, timeout time.Duration) Result {
		return Run(ctx, runner.Local{}, shell, timeout, now)
	}
	if r := run("id -un; grep -E '^(CapEff|NoNewPrivs)' /proc/self/status; echo $HOME", 10*time.Second); r.ExitCode != 0 ||
		!strings.HasPrefix(r.Output, "nobody\n") || !strings.Contains(r.Output, "CapEff:\t0000000000000000") ||
		!strings.Contains(r.Output, "NoNewPrivs:\t1") || !strings.HasSuffix(r.Output, "/\n") {
		t.Errorf("privileges not dropped: %+v", r)
	}
	if r := run("cat /etc/shadow", 10*time.Second); r.ExitCode == 0 || !strings.Contains(r.Output, "Permission denied") {
		t.Errorf("nobody can read /etc/shadow: %+v", r)
	}
	if r := run("touch /etc/linux-ha-agent-test", 10*time.Second); r.ExitCode == 0 {
		_ = os.Remove("/etc/linux-ha-agent-test")
		t.Errorf("nobody can write /etc: %+v", r)
	}
	if r := run("echo out; echo err >&2; exit 3", 10*time.Second); r.ExitCode != 3 || r.Output != "out\nerr\n" || r.Error != "" {
		t.Errorf("exit code or output: %+v", r)
	}
	if r := run("sleep 30", time.Second); r.Error == "" || !strings.Contains(r.Error, "timed out") {
		t.Errorf("timeout: %+v", r)
	}
	if r := run("yes", 10*time.Second); len(r.Output) != OutputLimit || r.Error == "" {
		t.Errorf("output limit: %d bytes, %q", len(r.Output), r.Error)
	}
}
