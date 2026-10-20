package incus

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/lvlie/incus-ha-agent/internal/runner"
)

// Fake is an in-memory Client for tests.
type Fake struct {
	mu        sync.Mutex
	guests    []Guest
	runners   map[string]*runner.Fake
	snapshots map[string][]Snapshot
	events    chan struct{}

	Restarts []string
	Restores []string
	// SnapshotErr makes CreateSnapshot fail.
	SnapshotErr error
}

// NewFake returns a Fake with the given guests. Each guest gets its own
// runner.Fake, available through FakeRunner.
func NewFake(guests ...Guest) *Fake {
	f := &Fake{runners: map[string]*runner.Fake{}, snapshots: map[string][]Snapshot{}, events: make(chan struct{}, 16)}
	f.SetGuests(guests...)
	return f
}

// SetGuests replaces the guest list and emits a lifecycle event.
func (f *Fake) SetGuests(guests ...Guest) {
	f.mu.Lock()
	f.guests = slices.Clone(guests)
	for _, g := range guests {
		if f.runners[g.Key()] == nil {
			f.runners[g.Key()] = &runner.Fake{}
		}
	}
	f.mu.Unlock()
	select {
	case f.events <- struct{}{}:
	default:
	}
}

// FakeRunner returns the scripted runner of a guest ("project/name").
func (f *Fake) FakeRunner(key string) *runner.Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runners[key] == nil {
		f.runners[key] = &runner.Fake{}
	}
	return f.runners[key]
}

// Guests implements Client.
func (f *Fake) Guests(context.Context) ([]Guest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.guests), nil
}

// Runner implements Client.
func (f *Fake) Runner(g Guest) runner.Runner {
	return f.FakeRunner(g.Key())
}

// CreateSnapshot implements Client.
func (f *Fake) CreateSnapshot(_ context.Context, g Guest, name string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.SnapshotErr != nil {
		return f.SnapshotErr
	}
	f.snapshots[g.Key()] = append([]Snapshot{{Name: name, CreatedAt: time.Now()}}, f.snapshots[g.Key()]...)
	return nil
}

// Snapshots implements Client.
func (f *Fake) Snapshots(_ context.Context, g Guest) ([]Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.snapshots[g.Key()]), nil
}

// RestoreSnapshot implements Client.
func (f *Fake) RestoreSnapshot(_ context.Context, g Guest, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.snapshots[g.Key()] {
		if s.Name == name {
			f.Restores = append(f.Restores, g.Key()+"@"+name)
			return nil
		}
	}
	return fmt.Errorf("snapshot %q of %s not found", name, g.Key())
}

// Restart implements Client.
func (f *Fake) Restart(_ context.Context, g Guest, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Restarts = append(f.Restarts, g.Key())
	return nil
}

// WatchLifecycle implements Client.
func (f *Fake) WatchLifecycle(ctx context.Context, fn func()) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-f.events:
			fn()
		}
	}
}
