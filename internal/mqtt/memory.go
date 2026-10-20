package mqtt

import (
	"strings"
	"sync"
)

// Memory is an in-memory Client for tests. It keeps retained messages and
// delivers publishes to matching subscriptions synchronously.
type Memory struct {
	mu       sync.Mutex
	retained map[string][]byte
	subs     map[string]Handler
	Log      []Message
}

// Message is a published message.
type Message struct {
	Topic    string
	Retained bool
	Payload  []byte
}

// NewMemory returns an empty Memory client.
func NewMemory() *Memory {
	return &Memory{retained: map[string][]byte{}, subs: map[string]Handler{}}
}

// Publish implements Client.
func (m *Memory) Publish(topic string, retained bool, payload []byte) error {
	m.mu.Lock()
	m.Log = append(m.Log, Message{Topic: topic, Retained: retained, Payload: append([]byte(nil), payload...)})
	if retained {
		if len(payload) == 0 {
			delete(m.retained, topic)
		} else {
			m.retained[topic] = append([]byte(nil), payload...)
		}
	}
	var handlers []Handler
	for filter, h := range m.subs {
		if Match(filter, topic) {
			handlers = append(handlers, h)
		}
	}
	m.mu.Unlock()
	for _, h := range handlers {
		h(topic, payload)
	}
	return nil
}

// Subscribe implements Client.
func (m *Memory) Subscribe(filter string, handler Handler) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs[filter] = handler
	return nil
}

// Retained returns the retained payload of a topic.
func (m *Memory) Retained(topic string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.retained[topic]
	return p, ok
}

// RetainedTopics returns all topics with a retained message matching filter.
func (m *Memory) RetainedTopics(filter string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var topics []string
	for t := range m.retained {
		if Match(filter, t) {
			topics = append(topics, t)
		}
	}
	return topics
}

// Match reports whether an MQTT topic matches a filter with + and # wildcards.
func Match(filter, topic string) bool {
	f := strings.Split(filter, "/")
	t := strings.Split(topic, "/")
	for i, part := range f {
		if part == "#" {
			return true
		}
		if i >= len(t) {
			return false
		}
		if part != "+" && part != t[i] {
			return false
		}
	}
	return len(f) == len(t)
}
