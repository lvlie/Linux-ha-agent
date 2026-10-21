// Package commands runs user-defined shell commands without privileges: as
// the user nobody, with no capabilities, no supplementary groups and
// no_new_privs set, so setuid programs such as sudo cannot gain any. They can
// read what every user may read, and nothing more.
package commands

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/lvlie/linux-ha-agent/internal/runner"
)

// OutputLimit is the most output kept of one command, stdout and stderr
// combined; the command is stopped when it writes more.
const OutputLimit = 64 * 1024

// ExitSetup is the exit code when the command could not be started without
// privileges (no user nobody or no setpriv); it never runs as root then.
const ExitSetup = 125

// wrapper runs as root on the target and drops privileges with setpriv
// (util-linux). It uses only POSIX sh, so it works on every distribution.
// $1 is the command, $2 the timeout in seconds and $3 the output limit.
const wrapper = `cd / || exit 125
u=$(id -u nobody 2>/dev/null) && g=$(id -g nobody 2>/dev/null) || { echo "linux-ha-agent: user nobody not found" >&2; exit 125; }
command -v setpriv >/dev/null 2>&1 || { echo "linux-ha-agent: setpriv not found (install util-linux)" >&2; exit 125; }
{ s=$( { { timeout -k 5 "$2" setpriv --reuid="$u" --regid="$g" --clear-groups --no-new-privs --inh-caps=-all -- sh -c "$1" 2>&1 </dev/null; echo $? >&3; } | head -c "$3" >&4; } 3>&1 ); } 4>&1
exit "${s:-1}"
`

// Command builds the command that runs shell as nobody.
func Command(shell string, timeout time.Duration) runner.Cmd {
	secs := max(int(timeout.Seconds()), 1)
	return runner.Command("sh", "-c", wrapper, "sh", shell, strconv.Itoa(secs), strconv.Itoa(OutputLimit)).
		WithEnv("HOME", "/", "USER", "nobody", "LOGNAME", "nobody")
}

// Result of a command.
type Result struct {
	Command  string    `json:"command"`
	Output   string    `json:"output"`
	ExitCode int       `json:"exit_code"`
	Time     time.Time `json:"time"`
	// Error is set when the command could not run or timed out.
	Error string `json:"error,omitempty"`
}

// Value is what a sensor shows: the first line of the output, at most 255
// characters (Home Assistant's limit for states).
func (r Result) Value() string {
	v, _, _ := strings.Cut(strings.TrimSpace(r.Output), "\n")
	v = strings.TrimSpace(v)
	if len(v) > 255 {
		v = v[:252] + "..."
	}
	return v
}

// Run runs shell as nobody on the target. The timeout includes the grace
// time the wrapper gives the command to exit.
func Run(ctx context.Context, r runner.Runner, shell string, timeout time.Duration, now func() time.Time) Result {
	ctx, cancel := context.WithTimeout(ctx, timeout+15*time.Second)
	defer cancel()
	res, err := r.Run(ctx, Command(shell, timeout))
	out := res.Stdout
	if len(out) > OutputLimit { // the wrapper limits it; guard against runners that do not
		out = out[:OutputLimit]
	}
	result := Result{Command: shell, Output: out, ExitCode: res.ExitCode, Time: now()}
	switch {
	case err != nil:
		result.ExitCode = -1
		result.Error = err.Error()
	case res.ExitCode == 124 || res.ExitCode == 137:
		result.Error = "timed out after " + timeout.String()
	case res.ExitCode == ExitSetup && strings.HasPrefix(res.Stderr, "linux-ha-agent:"):
		result.Error = strings.TrimSpace(strings.TrimPrefix(res.Stderr, "linux-ha-agent:"))
	case res.ExitCode == 141 && len(out) >= OutputLimit:
		result.Error = "output truncated at 64 KiB"
	}
	return result
}
