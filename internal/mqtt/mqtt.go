// Package mqtt is a small MQTT client wrapper with automatic reconnects,
// resubscription and a last will, plus an in-memory implementation for tests.
package mqtt

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/eclipse/paho.mqtt.golang/packets"

	"github.com/lvlie/linux-ha-agent/internal/config"
)

// Handler handles a received message.
type Handler func(topic string, payload []byte)

// Client is what the agent needs from MQTT.
type Client interface {
	Publish(topic string, retained bool, payload []byte) error
	Subscribe(filter string, handler Handler) error
}

// Options for Connect.
type Options struct {
	ClientID    string
	WillTopic   string
	WillPayload string
	// OnConnect runs after every (re)connect, once subscriptions are restored.
	OnConnect func()
	Logger    *slog.Logger
}

// Paho is a Client backed by the Eclipse Paho library.
type Paho struct {
	client paho.Client
	log    *slog.Logger

	mu   sync.Mutex
	subs map[string]Handler
}

const timeout = 30 * time.Second

// Connect connects to the broker. While the broker is unreachable it keeps
// retrying until ctx is done; a refusal (bad credentials, not authorised) is
// returned immediately. After the first connection, reconnects happen in the
// background.
func Connect(ctx context.Context, cfg config.MQTT, opts Options) (*Paho, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	p := &Paho{log: log, subs: map[string]Handler{}}

	o := paho.NewClientOptions().
		AddBroker(cfg.URL).
		SetClientID(opts.ClientID).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(time.Minute).
		SetKeepAlive(30 * time.Second).
		SetOrderMatters(false)
	if cfg.Username != "" {
		password, err := cfg.Password()
		if err != nil {
			return nil, err
		}
		o.SetUsername(cfg.Username).SetPassword(password)
	}
	if opts.WillTopic != "" {
		o.SetWill(opts.WillTopic, opts.WillPayload, 1, true)
	}
	tlsConfig, err := tlsConfig(cfg)
	if err != nil {
		return nil, err
	}
	if tlsConfig != nil {
		o.SetTLSConfig(tlsConfig)
	}
	o.SetConnectionLostHandler(func(_ paho.Client, err error) {
		log.Warn("mqtt connection lost", "err", err)
	})
	o.SetOnConnectHandler(func(paho.Client) {
		log.Info("mqtt connected", "broker", cfg.URL)
		if err := p.resubscribe(); err != nil {
			log.Error("mqtt resubscribe failed", "err", err)
		}
		if opts.OnConnect != nil {
			go opts.OnConnect()
		}
	})
	p.client = paho.NewClient(o)
	delay := time.Second
	for {
		tok := p.client.Connect()
		err := errors.New("timeout")
		if tok.WaitTimeout(timeout) {
			err = tok.Error()
		}
		if err == nil {
			return p, nil
		}
		if refused(err) {
			return nil, fmt.Errorf("mqtt broker refused the connection: %w", err)
		}
		log.Warn("mqtt broker not reachable, retrying", "broker", cfg.URL, "err", err, "retry_in", delay)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, time.Minute)
	}
}

func refused(err error) bool {
	return errors.Is(err, packets.ErrorRefusedBadUsernameOrPassword) ||
		errors.Is(err, packets.ErrorRefusedNotAuthorised) ||
		errors.Is(err, packets.ErrorRefusedIDRejected) ||
		errors.Is(err, packets.ErrorRefusedBadProtocolVersion)
}

func tlsConfig(cfg config.MQTT) (*tls.Config, error) {
	secure := strings.HasPrefix(cfg.URL, "ssl://") || strings.HasPrefix(cfg.URL, "tls://") ||
		strings.HasPrefix(cfg.URL, "mqtts://") || strings.HasPrefix(cfg.URL, "wss://")
	if !secure && cfg.CAFile == "" && cfg.ClientCertFile == "" {
		return nil, nil
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.InsecureSkipVerify} //nolint:gosec // opt-in for self-signed brokers
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading mqtt ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("mqtt ca_file contains no certificates")
		}
		tc.RootCAs = pool
	}
	if cfg.ClientCertFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.ClientCertFile, cfg.ClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("loading mqtt client certificate: %w", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	return tc, nil
}

// Publish implements Client. Everything the agent publishes is retained
// state that is republished in full after every reconnect, so messages are
// sent with QoS 0 and not queued while disconnected. This also keeps paho's
// in-flight store out of play: on reconnect paho resends stored QoS 1
// messages while its new connection may already be writing them (a data
// race in paho.mqtt.golang 1.5.1).
func (p *Paho) Publish(topic string, retained bool, payload []byte) error {
	if !p.client.IsConnectionOpen() {
		return fmt.Errorf("publishing to %s: not connected", topic)
	}
	tok := p.client.Publish(topic, 0, retained, payload)
	if !tok.WaitTimeout(timeout) {
		return fmt.Errorf("publishing to %s: timeout", topic)
	}
	return tok.Error()
}

// Subscribe implements Client. Subscriptions survive reconnects.
func (p *Paho) Subscribe(filter string, handler Handler) error {
	p.mu.Lock()
	p.subs[filter] = handler
	p.mu.Unlock()
	if !p.client.IsConnectionOpen() {
		return nil // subscribed on connect
	}
	return p.subscribe(filter, handler)
}

func (p *Paho) subscribe(filter string, handler Handler) error {
	tok := p.client.Subscribe(filter, 1, func(_ paho.Client, m paho.Message) {
		handler(m.Topic(), m.Payload())
	})
	if !tok.WaitTimeout(timeout) {
		return fmt.Errorf("subscribing to %s: timeout", filter)
	}
	return tok.Error()
}

func (p *Paho) resubscribe() error {
	p.mu.Lock()
	subs := make(map[string]Handler, len(p.subs))
	for k, v := range p.subs {
		subs[k] = v
	}
	p.mu.Unlock()
	var errs []error
	for filter, handler := range subs {
		errs = append(errs, p.subscribe(filter, handler))
	}
	return errors.Join(errs...)
}

// Disconnect publishes nothing further and closes the connection.
func (p *Paho) Disconnect() {
	p.client.Disconnect(1000)
}
