package mqtt

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"

	"github.com/lvlie/linux-ha-agent/internal/config"
)

func TestMatch(t *testing.T) {
	cases := []struct {
		filter, topic string
		want          bool
	}{
		{"a/b/c", "a/b/c", true},
		{"a/+/c", "a/x/c", true},
		{"a/+/c", "a/x/y/c", false},
		{"a/#", "a/x/y", true},
		{"a/#", "a", true}, // "#" also matches the parent level
		{"a/b", "a/b/c", false},
		{"a/b/c", "a/b", false},
		{"+/+/+/cmd", "base/host/dev/cmd", true},
	}
	for _, c := range cases {
		if got := Match(c.filter, c.topic); got != c.want {
			t.Errorf("Match(%q, %q) = %v", c.filter, c.topic, got)
		}
	}
}

func TestMemoryRetained(t *testing.T) {
	m := NewMemory()
	got := ""
	_ = m.Subscribe("x/+", func(topic string, payload []byte) { got = topic + "=" + string(payload) })
	_ = m.Publish("x/a", true, []byte("1"))
	if got != "x/a=1" {
		t.Fatalf("handler got %q", got)
	}
	if p, ok := m.Retained("x/a"); !ok || string(p) != "1" {
		t.Fatal("not retained")
	}
	_ = m.Publish("x/a", true, nil)
	if _, ok := m.Retained("x/a"); ok {
		t.Fatal("empty retained publish must clear the topic")
	}
}

// Broker is an embedded MQTT broker for tests.
type broker struct {
	server *mochi.Server
	addr   string
	once   sync.Once
}

// stop closes the broker once (mochi panics on a second Close).
func (b *broker) stop() {
	b.once.Do(func() { _ = b.server.Close() })
}

func startBroker(t *testing.T, ledger *auth.Ledger) *broker {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return startBrokerAt(t, addr, ledger)
}

// startBrokerAt starts a broker on addr, retrying while a previous broker
// on the same address releases it.
func startBrokerAt(t *testing.T, addr string, ledger *auth.Ledger) *broker {
	t.Helper()
	for i := 0; ; i++ {
		b, err := tryBroker(addr, ledger)
		if err == nil {
			t.Cleanup(b.stop)
			return b
		}
		if i == 50 {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func tryBroker(addr string, ledger *auth.Ledger) (*broker, error) {
	server := mochi.New(&mochi.Options{InlineClient: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if ledger == nil {
		_ = server.AddHook(new(auth.AllowHook), nil)
	} else if err := server.AddHook(new(auth.Hook), &auth.Options{Ledger: ledger}); err != nil {
		return nil, err
	}
	if err := server.AddListener(listeners.NewTCP(listeners.Config{ID: "tcp", Address: addr})); err != nil {
		_ = server.Close()
		return nil, err
	}
	go func() { _ = server.Serve() }()
	return &broker{server: server, addr: addr}, nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPahoWithPasswordAndWill(t *testing.T) {
	ledger := &auth.Ledger{Auth: auth.AuthRules{
		{Username: "agent", Password: "s3cret", Allow: true},
		{Username: "watcher", Password: "w", Allow: true},
	}}
	b := startBroker(t, ledger)
	pwFile := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(pwFile, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A wrong password is rejected.
	bad := filepath.Join(t.TempDir(), "bad")
	_ = os.WriteFile(bad, []byte("nope"), 0o600)
	if _, err := Connect(context.Background(), config.MQTT{URL: "tcp://" + b.addr, Username: "agent", PasswordFile: bad}, Options{ClientID: "bad"}); err == nil {
		t.Fatal("expected authentication failure")
	}

	watcher, err := Connect(context.Background(), config.MQTT{URL: "tcp://" + b.addr, Username: "watcher", PasswordFile: writeTemp(t, "w")}, Options{ClientID: "watcher"})
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Disconnect()
	status := make(chan string, 10)
	if err := watcher.Subscribe("agent/status", func(_ string, p []byte) { status <- string(p) }); err != nil {
		t.Fatal(err)
	}

	connects := make(chan struct{}, 10)
	agent, err := Connect(context.Background(), config.MQTT{URL: "tcp://" + b.addr, Username: "agent", PasswordFile: pwFile}, Options{
		ClientID: "agent", WillTopic: "agent/status", WillPayload: "offline",
		OnConnect: func() { connects <- struct{}{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Disconnect()
	<-connects
	got := make(chan string, 10)
	if err := agent.Subscribe("agent/cmd", func(_ string, p []byte) { got <- string(p) }); err != nil {
		t.Fatal(err)
	}
	if err := agent.Publish("agent/status", true, []byte("online")); err != nil {
		t.Fatal(err)
	}
	if s := <-status; s != "online" {
		t.Fatalf("status %q", s)
	}

	// Kill the connection without a DISCONNECT: the broker sends the will
	// and the client reconnects.
	cl, ok := b.server.Clients.Get("agent")
	if !ok {
		t.Fatal("agent not connected")
	}
	cl.Stop(errors.New("test: connection dropped"))
	waitFor(t, "last will", func() bool {
		select {
		case s := <-status:
			return s == "offline"
		default:
			return false
		}
	})
	select {
	case <-connects:
	case <-time.After(20 * time.Second):
		t.Fatal("client did not reconnect")
	}

	// Restart the broker. The new broker knows nothing about the client, so
	// receiving commands afterwards shows the client restored its own
	// subscriptions. (Reusing the killed session instead races with the
	// broker cleaning it up.)
	b.stop()
	startBrokerAt(t, b.addr, ledger)
	select {
	case <-connects:
	case <-time.After(30 * time.Second):
		t.Fatal("client did not reconnect to the restarted broker")
	}
	// The watcher was disconnected too and reconnects with backoff; a new
	// client does not depend on that timing.
	sender, err := Connect(context.Background(), config.MQTT{URL: "tcp://" + b.addr, Username: "watcher", PasswordFile: writeTemp(t, "w")}, Options{ClientID: "sender"})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Disconnect()
	waitFor(t, "command after reconnect", func() bool {
		_ = sender.Publish("agent/cmd", false, []byte("check"))
		select {
		case p := <-got:
			return p == "check"
		case <-time.After(200 * time.Millisecond):
			return false
		}
	})
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestUnreachableBrokerHonoursContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Connect(ctx, config.MQTT{URL: "tcp://127.0.0.1:1"}, Options{ClientID: "x", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 10*time.Second {
		t.Fatalf("expected context deadline after retrying, got %v after %v", err, time.Since(start))
	}
}

func TestTLSConfig(t *testing.T) {
	if tc, err := tlsConfig(config.MQTT{URL: "tcp://b:1883"}); err != nil || tc != nil {
		t.Fatalf("plain tcp must not use TLS: %v %v", tc, err)
	}
	tc, err := tlsConfig(config.MQTT{URL: "ssl://b:8883"})
	if err != nil || tc == nil || tc.InsecureSkipVerify {
		t.Fatalf("ssl must use verified TLS: %+v %v", tc, err)
	}
	if _, err := tlsConfig(config.MQTT{URL: "ssl://b:8883", CAFile: writeTemp(t, "not a cert")}); err == nil {
		t.Fatal("expected error for bad CA file")
	}
}
