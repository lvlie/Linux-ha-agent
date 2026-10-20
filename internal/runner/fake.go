package runner

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// Fake is a scripted Runner for tests. Responses are matched on the joined
// command line: the first rule whose pattern is contained in it wins.
type Fake struct {
	mu    sync.Mutex
	rules []fakeRule
	Calls []Cmd
}

type fakeRule struct {
	pattern string
	fn      func(Cmd) Result
}

// On returns res for every command containing pattern.
func (f *Fake) On(pattern string, res Result) *Fake {
	return f.OnFunc(pattern, func(Cmd) Result { return res })
}

// OnFunc computes the result for commands containing pattern.
func (f *Fake) OnFunc(pattern string, fn func(Cmd) Result) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, fakeRule{pattern: pattern, fn: fn})
	return f
}

// Run implements Runner. Unmatched commands exit with 127.
func (f *Fake) Run(_ context.Context, cmd Cmd) (Result, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, cmd)
	rules := append([]fakeRule(nil), f.rules...)
	f.mu.Unlock()
	line := cmd.String()
	for _, r := range rules {
		if strings.Contains(line, r.pattern) {
			return r.fn(cmd), nil
		}
	}
	return Result{ExitCode: 127, Stderr: fmt.Sprintf("fake: no rule for %q", line)}, nil
}

// Ran reports whether a command containing pattern was run.
func (f *Fake) Ran(pattern string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.Calls {
		if strings.Contains(c.String(), pattern) {
			return true
		}
	}
	return false
}
