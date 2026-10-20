package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"

	"github.com/lvlie/incus-ha-agent/internal/config"
	"github.com/lvlie/incus-ha-agent/internal/ha"
	"github.com/lvlie/incus-ha-agent/internal/incus"
	"github.com/lvlie/incus-ha-agent/internal/mqtt"
	"github.com/lvlie/incus-ha-agent/internal/runner"
)

// ledger mirrors the recommended broker ACL from the README: every agent
// authenticates with its host ID and may only touch its own topics.
func ledger() *auth.Ledger {
	agent := func(id string) auth.Filters {
		return auth.Filters{
			auth.RString("homeassistant/device/" + id + "/#"): auth.WriteOnly,
			auth.RString("incus-ha-agent/" + id + "/#"):       auth.ReadWrite,
			"homeassistant/status":                            auth.ReadOnly,
			// mochi allows unmatched topics; mosquitto denies them. Deny explicitly.
			"#": auth.Deny,
		}
	}
	return &auth.Ledger{
		Auth: auth.AuthRules{
			{Username: "lab1", Password: "pw1", Allow: true},
			{Username: "lab2", Password: "pw2", Allow: true},
			{Username: "homeassistant", Password: "ha", Allow: true},
		},
		ACL: auth.ACLRules{
			{Username: "lab1", Filters: agent("lab1")},
			{Username: "lab2", Filters: agent("lab2")},
			{Username: "homeassistant"},
		},
	}
}

func startBroker(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	server := mochi.New(&mochi.Options{InlineClient: true, Logger: quiet})
	if err := server.AddHook(new(auth.Hook), &auth.Options{Ledger: ledger()}); err != nil {
		t.Fatal(err)
	}
	if err := server.AddListener(listeners.NewTCP(listeners.Config{ID: "tcp", Address: addr})); err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve() }()
	t.Cleanup(func() { _ = server.Close() })
	return "tcp://" + addr
}

type recorder struct {
	mu   sync.Mutex
	msgs map[string][]byte
}

func (r *recorder) handle(topic string, payload []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs[topic] = append([]byte(nil), payload...)
}

func (r *recorder) get(topic string) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.msgs[topic]
	return p, ok
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func connect(t *testing.T, url, user, password string) *mqtt.Paho {
	t.Helper()
	pw := filepath.Join(t.TempDir(), "pw")
	writeFile(t, pw, password)
	c, err := mqtt.Connect(context.Background(), config.MQTT{URL: url, Username: user, PasswordFile: pw},
		mqtt.Options{ClientID: user, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Disconnect)
	return c
}

func TestAgentOverRealMQTTWithACL(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	url := startBroker(t)

	homeAssistant := connect(t, url, "homeassistant", "ha")
	seen := &recorder{msgs: map[string][]byte{}}
	if err := homeAssistant.Subscribe("#", seen.handle); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.MQTT.URL = url
	cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	cfg.Permissions = config.Permissions{Upgrade: true, Reboot: true}
	m := &machine{}
	inc := incus.NewFake(incus.Guest{Name: "web", Project: "default", Type: "container", Status: "Running"})
	m.ubuntu(inc.FakeRunner("default/web"))
	host := &runner.Fake{}
	(&machine{}).ubuntu(host)

	client := connect(t, url, "lab1", "pw1")
	a, err := New(Options{Config: cfg, MQTT: client, Incus: inc, Local: host, Hostname: "lab1", Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Run(ctx) }()

	eventually(t, "discovery of the guest", func() bool {
		_, ok := seen.get("homeassistant/device/lab1/lab1_web/config")
		return ok
	})
	eventually(t, "guest checked", func() bool {
		p, _ := seen.get("incus-ha-agent/lab1/lab1_web/state")
		return strings.Contains(string(p), `"updates":2`)
	})

	// Another agent must not be able to command lab1's guests.
	intruder := connect(t, url, "lab2", "pw2")
	_ = intruder.Publish("incus-ha-agent/lab1/lab1_web/cmd", false, []byte(ha.CmdReboot))
	_ = intruder.Publish("homeassistant/device/lab1/lab1_web/config", true, []byte(`{"spoofed":true}`))
	time.Sleep(300 * time.Millisecond)
	a.WaitIdle()
	if len(inc.Restarts) != 0 {
		t.Fatal("command from another agent was executed")
	}
	if p, _ := seen.get("homeassistant/device/lab1/lab1_web/config"); strings.Contains(string(p), "spoofed") {
		t.Fatal("another agent could overwrite lab1's discovery")
	}

	// Home Assistant can.
	_ = homeAssistant.Publish("incus-ha-agent/lab1/lab1_web/cmd", false, []byte(ha.CmdUpgrade))
	eventually(t, "upgrade result", func() bool {
		p, _ := seen.get("incus-ha-agent/lab1/lab1_web/state")
		var st map[string]any
		return json.Unmarshal(p, &st) == nil && st["last_run_result"] == "upgraded" && st["in_progress"] == false
	})

	cancel()
	eventually(t, "offline status", func() bool {
		p, _ := seen.get("incus-ha-agent/lab1/status")
		return string(p) == "offline"
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := writeFileErr(path, content); err != nil {
		t.Fatal(err)
	}
}

func writeFileErr(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
