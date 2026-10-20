// Command incus-ha-agent publishes update information for a Linux host and
// its Incus guests to Home Assistant over MQTT, and runs upgrades on request
// or on a schedule.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lvlie/incus-ha-agent/internal/agent"
	"github.com/lvlie/incus-ha-agent/internal/config"
	"github.com/lvlie/incus-ha-agent/internal/containers"
	"github.com/lvlie/incus-ha-agent/internal/incus"
	"github.com/lvlie/incus-ha-agent/internal/mqtt"
	"github.com/lvlie/incus-ha-agent/internal/runner"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const defaultIncusSocket = "/var/lib/incus/unix.socket"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "incus-ha-agent:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/etc/incus-ha-agent/config.yaml", "configuration file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	checkConfig := flag.Bool("check-config", false, "validate the configuration and exit")
	once := flag.Bool("once", false, "check all devices once, print their state as JSON and exit (no MQTT)")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *checkConfig {
		fmt.Println("configuration is valid")
		return nil
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	hostname, err := os.Hostname()
	if err != nil {
		return err
	}
	incusClient, err := connectIncus(cfg.Incus, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	opts := agent.Options{
		Config:   cfg,
		Incus:    incusClient,
		Local:    runner.Local{},
		Registry: &containers.Registry{},
		Hostname: hostname,
		Version:  version,
		Logger:   log,
	}
	if *once {
		return runOnce(ctx, opts)
	}

	var a *agent.Agent
	ready := make(chan struct{})
	client, err := mqtt.Connect(ctx, cfg.MQTT, mqtt.Options{
		ClientID:    clientID(cfg, hostname),
		WillTopic:   cfg.HomeAssistant.BaseTopic + "/" + hostID(cfg, hostname) + "/status",
		WillPayload: "offline",
		Logger:      log,
		OnConnect: func() {
			<-ready
			a.Reconnected()
		},
	})
	if err != nil {
		return err
	}
	defer client.Disconnect()
	opts.MQTT = client
	a, err = agent.New(opts)
	if err != nil {
		return err
	}
	close(ready)
	log.Info("incus-ha-agent starting", "version", version, "host", hostname, "incus", incusClient != nil)
	return a.Run(ctx)
}

func hostID(cfg config.Config, hostname string) string {
	name := cfg.Host.Name
	if name == "" {
		name = hostname
	}
	return agent.HostID(name)
}

func clientID(cfg config.Config, hostname string) string {
	if cfg.MQTT.ClientID != "" {
		return cfg.MQTT.ClientID
	}
	return "incus-ha-agent-" + hostID(cfg, hostname)
}

func connectIncus(cfg config.Incus, log *slog.Logger) (incus.Client, error) {
	if cfg.Enabled == "false" {
		return nil, nil
	}
	socket := cfg.Socket
	if socket == "" {
		socket = os.Getenv("INCUS_SOCKET")
	}
	if socket == "" {
		socket = defaultIncusSocket
	}
	if cfg.Enabled == "auto" {
		if _, err := os.Stat(socket); errors.Is(err, fs.ErrNotExist) { //nolint:gosec // the socket path comes from the configuration
			log.Info("no incus socket found, managing only this machine", "socket", socket)
			return nil, nil
		}
	}
	client, err := incus.Connect(socket)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func runOnce(ctx context.Context, opts agent.Options) error {
	memory := mqtt.NewMemory()
	opts.MQTT = memory
	// Keep the real state file untouched.
	dir, err := os.MkdirTemp("", "incus-ha-agent-once")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	opts.Config.StateFile = filepath.Join(dir, "state.json")
	a, err := agent.New(opts)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*opts.Config.Upgrade.CommandTimeout+time.Minute)
	defer cancel()
	if err := a.Start(ctx); err != nil {
		return err
	}
	a.WaitIdle()
	out := map[string]json.RawMessage{}
	for _, topic := range memory.RetainedTopics(a.Topics().Base + "/" + a.Topics().HostID + "/+/state") {
		payload, _ := memory.Retained(topic)
		parts := strings.Split(topic, "/")
		out[parts[len(parts)-2]] = payload
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
