// kernclip_bus.go — KernClip Bus SDK for Go
// Zero external dependencies. stdlib only.
//
// Usage:
//   bus := kernclip_bus.New()
//   bus.Pub("my-topic", "hello world", "text/plain")
//   msg, _ := bus.Get("my-topic")
//   bus.Sub(ctx, "my-topic", func(msg Message) { fmt.Println(msg.Data) })

package kernclip_bus

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
)

func defaultSocket() string {
	return fmt.Sprintf("/run/user/%d/kernclip-bus.sock", os.Getuid())
}

type BusError struct{ msg string }

func (e *BusError) Error() string { return e.msg }

type Message struct {
	Topic  string `json:"topic"`
	Mime   string `json:"mime"`
	Data   string `json:"data"`
	Seq    uint64 `json:"seq"`
	Ts     int64  `json:"ts"`
	Sender string `json:"sender"`
}

type Bus struct {
	SocketPath string
}

func New() *Bus                        { return &Bus{SocketPath: defaultSocket()} }
func NewAt(path string) *Bus           { return &Bus{SocketPath: path} }

func (b *Bus) connect() (net.Conn, error) {
	c, err := net.Dial("unix", b.SocketPath)
	if err != nil {
		return nil, &BusError{fmt.Sprintf(
			"kernclip-busd not running? Start with: kernclip-busd\nSocket: %s\nError: %v",
			b.SocketPath, err,
		)}
	}
	return c, nil
}

func (b *Bus) roundtrip(req any) (map[string]any, error) {
	c, err := b.connect()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return nil, err
	}
	var resp map[string]any
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return nil, err
	}
	if ok, _ := resp["ok"].(bool); !ok {
		msg, _ := resp["error"].(string)
		return nil, &BusError{msg}
	}
	return resp, nil
}

// Pub publishes data to a topic. Returns assigned seq number.
func (b *Bus) Pub(topic, data, mime string) (uint64, error) {
	if mime == "" {
		mime = "text/plain"
	}
	r, err := b.roundtrip(map[string]any{
		"op": "pub", "topic": topic, "mime": mime, "data": data,
	})
	if err != nil {
		return 0, err
	}
	seq, _ := r["seq"].(float64)
	return uint64(seq), nil
}

// Get returns the latest message on a topic, or nil if empty.
func (b *Bus) Get(topic string) (*Message, error) {
	r, err := b.roundtrip(map[string]any{"op": "get", "topic": topic})
	if err != nil {
		return nil, err
	}
	data, _ := r["data"].(string)
	if data == "" {
		return nil, nil
	}
	msg := &Message{}
	msg.Topic, _ = r["topic"].(string)
	msg.Mime, _  = r["mime"].(string)
	msg.Data      = data
	msg.Sender, _ = r["sender"].(string)
	if seq, ok := r["seq"].(float64); ok {
		msg.Seq = uint64(seq)
	}
	if ts, ok := r["ts"].(float64); ok {
		msg.Ts = int64(ts)
	}
	return msg, nil
}

// Since returns all messages with seq > afterSeq.
func (b *Bus) Since(topic string, afterSeq uint64) ([]Message, error) {
	r, err := b.roundtrip(map[string]any{
		"op": "get", "topic": topic, "after_seq": afterSeq,
	})
	if err != nil {
		return nil, err
	}
	raw, _ := r["messages"].([]any)
	out := make([]Message, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			var msg Message
			msg.Topic, _  = m["topic"].(string)
			msg.Mime, _   = m["mime"].(string)
			msg.Data, _   = m["data"].(string)
			msg.Sender, _ = m["sender"].(string)
			if seq, ok := m["seq"].(float64); ok { msg.Seq = uint64(seq) }
			if ts, ok := m["ts"].(float64); ok   { msg.Ts = int64(ts) }
			out = append(out, msg)
		}
	}
	return out, nil
}

// Sub subscribes to a topic, calling fn for each new message.
// Blocks until ctx is cancelled or connection drops.
func (b *Bus) Sub(ctx context.Context, topic string, fn func(Message)) error {
	c, err := b.connect()
	if err != nil {
		return err
	}
	defer c.Close()

	go func() {
		<-ctx.Done()
		c.Close()
	}()

	if err := json.NewEncoder(c).Encode(map[string]any{"op": "sub", "topic": topic}); err != nil {
		return err
	}

	scanner := bufio.NewScanner(c)
	for scanner.Scan() {
		var r map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			continue
		}
		if ok, _ := r["ok"].(bool); !ok {
			continue
		}
		var msg Message
		msg.Topic, _  = r["topic"].(string)
		msg.Mime, _   = r["mime"].(string)
		msg.Data, _   = r["data"].(string)
		msg.Sender, _ = r["sender"].(string)
		if seq, ok := r["seq"].(float64); ok { msg.Seq = uint64(seq) }
		if ts, ok := r["ts"].(float64); ok   { msg.Ts = int64(ts) }
		fn(msg)
	}
	return scanner.Err()
}

// ── Subscription with pattern ──────────────────────────────────────────────────

// SubPattern subscribes to topics matching a glob pattern.
// Pattern syntax: * matches one segment, # matches one+ segments (terminal)
func (b *Bus) SubPattern(ctx context.Context, pattern string, fn func(Message)) error {
	c, err := b.connect()
	if err != nil {
		return err
	}
	defer c.Close()

	go func() {
		<-ctx.Done()
		c.Close()
	}()

	if err := json.NewEncoder(c).Encode(map[string]any{"op": "sub", "pattern": pattern}); err != nil {
		return err
	}

	scanner := bufio.NewScanner(c)
	for scanner.Scan() {
		var r map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			continue
		}
		if ok, _ := r["ok"].(bool); !ok {
			continue
		}
		var msg Message
		msg.Topic, _  = r["topic"].(string)
		msg.Mime, _   = r["mime"].(string)
		msg.Data, _   = r["data"].(string)
		msg.Sender, _ = r["sender"].(string)
		if seq, ok := r["seq"].(float64); ok { msg.Seq = uint64(seq) }
		if ts, ok := r["ts"].(float64); ok   { msg.Ts = int64(ts) }
		fn(msg)
	}
	return scanner.Err()
}

// SubMulti subscribes to multiple topics (fan-in).
func (b *Bus) SubMulti(ctx context.Context, topics []string, fn func(Message)) error {
	c, err := b.connect()
	if err != nil {
		return err
	}
	defer c.Close()

	go func() {
		<-ctx.Done()
		c.Close()
	}()

	if err := json.NewEncoder(c).Encode(map[string]any{"op": "sub", "topics": topics}); err != nil {
		return err
	}

	scanner := bufio.NewScanner(c)
	for scanner.Scan() {
		var r map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			continue
		}
		if ok, _ := r["ok"].(bool); !ok {
			continue
		}
		var msg Message
		msg.Topic, _  = r["topic"].(string)
		msg.Mime, _   = r["mime"].(string)
		msg.Data, _   = r["data"].(string)
		msg.Sender, _ = r["sender"].(string)
		if seq, ok := r["seq"].(float64); ok { msg.Seq = uint64(seq) }
		if ts, ok := r["ts"].(float64); ok   { msg.Ts = int64(ts) }
		fn(msg)
	}
	return scanner.Err()
}

// ── Topic management ───────────────────────────────────────────────────────────

// CreateTopic creates a topic with optional ring size override.
// Returns (ringSize, created, error).
func (b *Bus) CreateTopic(topic string, ringSize int) (int, bool, error) {
	req := map[string]any{"op": "create_topic", "topic": topic}
	if ringSize > 0 {
		req["ring_size"] = ringSize
	}
	r, err := b.roundtrip(req)
	if err != nil {
		return 0, false, err
	}
	rs, _ := r["ring_size"].(float64)
	created, _ := r["created"].(bool)
	return int(rs), created, nil
}

// Manifest returns the daemon's self-describing schema.
func (b *Bus) Manifest() (map[string]any, error) {
	r, err := b.roundtrip(map[string]any{"op": "manifest"})
	if err != nil {
		return nil, err
	}
	m, _ := r["manifest"].(map[string]any)
	return m, nil

}

// Topics lists all active topics.
func (b *Bus) Topics() ([]string, error) {
	r, err := b.roundtrip(map[string]any{"op": "topics"})
	if err != nil {
		return nil, err
	}
	raw, _ := r["topics"].([]any)
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		if s, ok := t.(string); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// Clipboard shortcuts
func (b *Bus) GetClipboard() (string, error) {
	msg, err := b.Get("kernclip.clipboard")
	if err != nil || msg == nil {
		return "", err
	}
	return msg.Data, nil
}

func (b *Bus) SetClipboard(text string) error {
	_, err := b.Pub("kernclip.clipboard", text, "text/plain")
	return err
}
