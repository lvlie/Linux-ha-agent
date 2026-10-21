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

// wrapper runs as root on the target and drops privileges with setpriv.
// util-linux's setpriv changes user and group itself; BusyBox's (Alpine) can
// only set no_new_privs, so there su switches to nobody. The command runs in
// its own session: on timeout the whole process group is killed, and so is
// anything it leaves behind, so nothing keeps running or holds the output
// open. It uses only POSIX sh, so it works on every distribution. $1 is the
// command, $2 the timeout in seconds and $3 the output limit.
const wrapper = `c=$1 t=$2 l=$3
cd / || exit 125
u=$(id -u nobody 2>/dev/null) && g=$(id -g nobody 2>/dev/null) || { echo "linux-ha-agent: user nobody not found" >&2; exit 125; }
command -v setsid >/dev/null 2>&1 || { echo "linux-ha-agent: setsid not found (install util-linux)" >&2; exit 125; }
help=$(setpriv --help 2>&1) || :
case $help in
*--reuid*) set -- setpriv --reuid="$u" --regid="$g" --clear-groups --no-new-privs --inh-caps=-all -- sh -c "$c" ;;
*--nnp*) command -v su >/dev/null 2>&1 || { echo "linux-ha-agent: su not found" >&2; exit 125; }
	set -- setpriv --nnp su -s /bin/sh nobody -c "$c" ;;
*) echo "linux-ha-agent: setpriv not found (install util-linux)" >&2; exit 125 ;;
esac
run() {
	setsid "$@" </dev/null 2>&1 3>&- 4>&- &
	p=$!
	setsid sh -c 'sleep "$1"; kill -s TERM -- "-$2"; sleep 5; kill -s KILL -- "-$2"' sh "$t" "$p" </dev/null >/dev/null 2>&1 3>&- 4>&- &
	k=$!
	wait "$p"
	r=$?
	kill -s KILL -- "-$k" 2>/dev/null
	kill -s KILL -- "-$p" 2>/dev/null
	return "$r"
}
{ s=$( { { run "$@"; echo $? >&3; } | head -c "$l" >&4; } 3>&1 ); } 4>&1
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
	start := time.Now()
	res, err := r.Run(ctx, Command(shell, timeout))
	elapsed := time.Since(start)
	out := res.Stdout
	if len(out) > OutputLimit { // the wrapper limits it; guard against runners that do not
		out = out[:OutputLimit]
	}
	result := Result{Command: shell, Output: out, ExitCode: res.ExitCode, Time: now()}
	switch {
	case err != nil:
		result.ExitCode = -1
		result.Error = err.Error()
	case elapsed >= timeout && (res.ExitCode == 137 || res.ExitCode == 143):
		result.Error = "timed out after " + timeout.String()
	case res.ExitCode == ExitSetup && strings.HasPrefix(res.Stderr, "linux-ha-agent:"):
		result.Error = strings.TrimSpace(strings.TrimPrefix(res.Stderr, "linux-ha-agent:"))
	case res.ExitCode == 141 && len(out) >= OutputLimit:
		result.Error = "output truncated at 64 KiB"
	}
	return result
}
