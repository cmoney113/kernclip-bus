package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// ── Message envelope ──────────────────────────────────────────────────────────

type Message struct {
	Seq       uint64 `json:"seq"`
	Timestamp int64  `json:"ts"` // unix ms
	Topic     string `json:"topic"`
	Mime      string `json:"mime"`
	Sender    string `json:"sender"`
	Data      string `json:"data"` // raw string; callers base64 if binary
}

// ── Topic ring buffer ─────────────────────────────────────────────────────────

type topic struct {
	mu        sync.RWMutex
	name      string
	ring      []Message
	cap       int
	head      int    // next write position
	count     int    // messages stored (≤ cap)
	nextSeq   uint64
	subs      map[uint64]chan Message
	nextSubID uint64
	// Overflow tracking
	wrapped   bool   // has the ring ever wrapped?
	oldestSeq uint64 // seq of oldest message currently in ring
}

func newTopic(name string, capacity int) *topic {
	if capacity <= 0 {
		capacity = 128
	}
	return &topic{
		name: name,
		ring: make([]Message, capacity),
		cap:  capacity,
		subs: make(map[uint64]chan Message),
	}
}

func (t *topic) publish(msg Message) Message {
	t.mu.Lock()
	msg.Seq = t.nextSeq
	t.nextSeq++
	msg.Timestamp = time.Now().UnixMilli()
	t.ring[t.head] = msg
	t.head = (t.head + 1) % t.cap
	if t.count < t.cap {
		t.count++
	}
	// Track overflow
	if t.count == t.cap {
		t.wrapped = true
		// Oldest seq is now (nextSeq - cap)
		t.oldestSeq = t.nextSeq - uint64(t.cap)
	}
	// snapshot subscribers to notify outside lock
	chans := make([]chan Message, 0, len(t.subs))
	for _, ch := range t.subs {
		chans = append(chans, ch)
	}
	t.mu.Unlock()

	for _, ch := range chans {
		select {
		case ch <- msg:
		default: // subscriber too slow — drop rather than block
		}
	}
	return msg
}

// GetResult holds messages plus overflow metadata.
type GetResult struct {
	Messages  []Message
	Overflow  bool   // true if some history was dropped from ring
	OldestSeq uint64 // oldest seq currently in ring
}

// since returns messages with seq > afterSeq plus overflow metadata.
func (t *topic) since(afterSeq uint64) GetResult {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []Message
	// walk ring oldest→newest
	start := 0
	if t.count == t.cap {
		start = t.head // oldest slot when full
	}
	for i := 0; i < t.count; i++ {
		m := t.ring[(start+i)%t.cap]
		if m.Seq > afterSeq {
			out = append(out, m)
		}
	}
	return GetResult{
		Messages:  out,
		Overflow:  t.wrapped && afterSeq < t.oldestSeq,
		OldestSeq: t.oldestSeq,
	}
}

// latest returns the most recently published message, or false if empty.
func (t *topic) latest() (Message, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.count == 0 {
		return Message{}, false
	}
	idx := (t.head - 1 + t.cap) % t.cap
	return t.ring[idx], true
}

func (t *topic) subscribe() (uint64, chan Message) {
	ch := make(chan Message, 64)
	t.mu.Lock()
	id := t.nextSubID
	t.nextSubID++
	t.subs[id] = ch
	t.mu.Unlock()
	return id, ch
}

func (t *topic) unsubscribe(id uint64) {
	t.mu.Lock()
	delete(t.subs, id)
	t.mu.Unlock()
}

// info returns topic metadata for manifest.
func (t *topic) info() map[string]interface{} {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return map[string]interface{}{
		"topic":      t.name,
		"ring_size":  t.cap,
		"msg_count":  t.nextSeq,
		"latest_seq": func() uint64 { if t.nextSeq > 0 { return t.nextSeq - 1 }; return 0 }(),
		"oldest_seq": t.oldestSeq,
		"wrapped":    t.wrapped,
	}
}
type Bus struct {
	mu       sync.RWMutex
	topics   map[string]*topic
	config   Config
	patterns *PatternRegistry
}

func newBus() *Bus {
	cfg := DefaultConfig
	b := &Bus{
		topics:   make(map[string]*topic),
		config:   cfg,
		patterns: NewPatternRegistry(),
	}
	// built-in clipboard topic
	b.getOrCreate("kernclip.clipboard")
	return b
}

func newBusWithConfig(cfg Config) *Bus {
	b := &Bus{
		topics:   make(map[string]*topic),
		config:   cfg,
		patterns: NewPatternRegistry(),
	}
	// built-in clipboard topic
	b.getOrCreate("kernclip.clipboard")
	return b
}

func (b *Bus) getOrCreate(name string) *topic {
	b.mu.RLock()
	t, ok := b.topics[name]
	b.mu.RUnlock()
	if ok {
		return t
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if t, ok = b.topics[name]; ok {
		return t
	}
	size := b.config.RingSizeFor(name)
	t = newTopic(name, size)
	b.topics[name] = t
	return t
}

func (b *Bus) createTopic(name string, size int) (*topic, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t, ok := b.topics[name]; ok {
		return t, false // already exists
	}
	if size <= 0 {
		size = b.config.RingSizeFor(name)
	}
	t := newTopic(name, size)
	b.topics[name] = t
	return t, true
}

func (b *Bus) topicNames() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	names := make([]string, 0, len(b.topics))
	for n := range b.topics {
		names = append(names, n)
	}
	return names
}

func (b *Bus) topicInfos() []map[string]interface{} {
	b.mu.RLock()
	defer b.mu.RUnlock()
	infos := make([]map[string]interface{}, 0, len(b.topics))
	for _, t := range b.topics {
		info := t.info()
		info["patterns"] = b.patterns.MatchingPatterns(t.name)
		infos = append(infos, info)
	}
	return infos
}


// ── JSON gateway ──────────────────────────────────────────────────────────────

type request struct {
	Op        string  `json:"op"`
	Topic     string  `json:"topic"`
	Mime      string  `json:"mime"`
	Data      string  `json:"data"`
	AfterSeq  *uint64 `json:"after_seq"`
	Limit     int     `json:"limit"`
	TimeoutMs int     `json:"timeout_ms"`
	SenderName string `json:"sender_name"`
}

// OpParam describes a single parameter of a bus operation.
type OpParam struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
	Default     string `json:"default,omitempty"`
}

// OpDef describes one bus operation for the manifest.
type OpDef struct {
	Op          string             `json:"op"`
	ToolName    string             `json:"tool_name"`
	Description string             `json:"description"`
	Streaming   bool               `json:"streaming"`
	Params      map[string]OpParam `json:"params"`
}

// Manifest is the self-description returned by {"op":"manifest"}.
type Manifest struct {
	Version string  `json:"version"`
	Socket  string  `json:"socket"`
	Ops     []OpDef `json:"ops"`
}

type response struct {
	OK       bool        `json:"ok"`
	Error    string      `json:"error,omitempty"`
	Topic    string      `json:"topic,omitempty"`
	Mime     string      `json:"mime,omitempty"`
	Seq      uint64      `json:"seq"`
	Ts       int64       `json:"ts,omitempty"`
	Data     string      `json:"data,omitempty"`
	Sender   string      `json:"sender,omitempty"`
	Topics   []string    `json:"topics,omitempty"`
	Messages []Message   `json:"messages,omitempty"`
	Manifest *Manifest   `json:"manifest,omitempty"`
}

func sender(c net.Conn) string {
	if a := c.RemoteAddr(); a != nil {
		return a.String()
	}
	return "unknown"
}

// buildManifest returns the self-describing manifest of all bus operations.
// This is the source of truth for the MCP server — add a new op here and
// the MCP server exposes it automatically with no other changes needed.
func buildManifest(bus *Bus) *Manifest {
	return &Manifest{
		Version: "1.1.0",
		Socket:  socketPath(),
		Ops: []OpDef{
			{
				Op:          "pub",
				ToolName:    "bus_pub",
				Description: "Publish data to a named topic. Any process, any language can subscribe and receive it.",
				Params: map[string]OpParam{
					"topic":       {Type: "string", Required: true, Description: "Topic name (e.g. 'ai.session.001.output')"},
					"data":        {Type: "string", Required: true, Description: "Payload to publish"},
					"mime":        {Type: "string", Required: false, Default: "text/plain", Description: "MIME type (e.g. 'application/json')"},
					"sender_name": {Type: "string", Required: false, Description: "Human-readable sender identity"},
				},
			},
			{
				Op:          "get",
				ToolName:    "bus_get",
				Description: "Read the latest message from a topic, or all messages since a given seq number.",
				Params: map[string]OpParam{
					"topic":     {Type: "string", Required: true, Description: "Topic name"},
					"after_seq": {Type: "integer", Required: false, Description: "Return all messages with seq > N (omit for latest only)"},
				},
			},
			{
				Op:          "poll",
				ToolName:    "bus_poll",
				Description: "Wait up to timeout_ms for new messages after after_seq. Returns immediately if messages are already available. MCP-friendly alternative to sub.",
				Params: map[string]OpParam{
					"topic":      {Type: "string", Required: true, Description: "Topic name"},
					"after_seq":  {Type: "integer", Required: false, Default: "0", Description: "Only return messages with seq > N"},
					"limit":      {Type: "integer", Required: false, Default: "10", Description: "Max messages to return"},
					"timeout_ms": {Type: "integer", Required: false, Default: "2000", Description: "How long to wait for messages (ms)"},
				},
			},
			{
				Op:          "topics",
				ToolName:    "bus_topics",
				Description: "List all active topic names on the bus.",
				Params:      map[string]OpParam{},
			},
			{
				Op:          "create_topic",
				ToolName:    "bus_create_topic",
				Description: "Explicitly create a topic with optional ring size override.",
				Params: map[string]OpParam{
					"topic":     {Type: "string", Required: true, Description: "Topic name to create"},
					"ring_size": {Type: "integer", Required: false, Description: "Override ring buffer size (default: from config)"},
				},
			},
			{
				Op:          "manifest",
				ToolName:    "bus_manifest",
				Description: "Return this manifest — the self-description of all bus operations and their schemas.",
				Params:      map[string]OpParam{},
			},
			{
				Op:          "terminal_context",
				ToolName:    "bus_terminal_context",
				Description: "Fetch the last terminal command and its output in one shot. Returns {command, output, command_seq, output_seq} — everything a model needs to see what just ran in the user's terminal.",
				Params:      map[string]OpParam{},
			},
			{
				Op:          "tail",
				ToolName:    "bus_tail",
				Description: "Return the last N messages from a topic's ring buffer and exit. Like unix tail. Useful for reviewing recent history on any topic.",
				Params: map[string]OpParam{
					"topic": {Type: "string", Required: true, Description: "Topic name"},
					"n":     {Type: "integer", Required: false, Default: "10", Description: "Number of messages to return (default 10)"},
				},
			},
			{
				Op:          "wait",
				ToolName:    "bus_wait",
				Description: "Block until the next new message arrives on a topic, then return it. Semantically cleaner than poll when you want exactly the next thing published, not history.",
				Params: map[string]OpParam{
					"topic":      {Type: "string", Required: true, Description: "Topic name"},
					"timeout_ms": {Type: "integer", Required: false, Default: "30000", Description: "Give up after N ms (default 30s)"},
				},
			},
			{
				Op:          "copy",
				ToolName:    "bus_copy",
				Description: "Copy text to the bus clipboard (publishes to kernclip.clipboard). Equivalent to pub with topic hardcoded.",
				Params: map[string]OpParam{
					"data": {Type: "string", Required: true, Description: "Text to copy to the clipboard"},
				},
			},
			{
				Op:          "paste",
				ToolName:    "bus_paste",
				Description: "Read the current value of the bus clipboard (reads from kernclip.clipboard). Returns empty string if clipboard is empty.",
				Params:      map[string]OpParam{},
			},
			{
				Op:          "status",
				ToolName:    "bus_status",
				Description: "Return daemon health and active topic count. Useful for diagnostics — call this first if other tools are failing.",
				Params:      map[string]OpParam{},
			},
		},
	}
}

func handleConn(conn net.Conn, bus *Bus) {
	defer conn.Close()
	connID := conn.RemoteAddr().String()
	pid := fmt.Sprintf("%d", os.Getpid()) // proxy; real sender could send pid in request
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	reply := func(r response) {
		enc.Encode(r) //nolint:errcheck
	}
	fail := func(msg string) {
		reply(response{OK: false, Error: msg})
	}

	for {
		var rawReq map[string]interface{}
		if err := dec.Decode(&rawReq); err != nil {
			if err != io.EOF {
				log.Printf("decode error from %s: %v", sender(conn), err)
			}
			return
		}

		// Parse op
		op, _ := rawReq["op"].(string)
		switch op {

		case "pub":
			topic, _ := rawReq["topic"].(string)
			if topic == "" {
				fail("missing topic")
				continue
			}
			mime, _ := rawReq["mime"].(string)
			if mime == "" {
				mime = "text/plain"
			}
			data, _ := rawReq["data"].(string)
			senderName, _ := rawReq["sender_name"].(string)
			if senderName == "" {
				senderName = pid
			}
			t := bus.getOrCreate(topic)
			msg := Message{Topic: topic, Mime: mime, Data: data, Sender: senderName}
			msg = t.publish(msg)
			// Fan out to pattern subscribers
			bus.patterns.FanOut(topic, msg)
			reply(response{OK: true, Topic: topic, Seq: msg.Seq})

		case "get":
			topic, _ := rawReq["topic"].(string)
			if topic == "" {
				fail("missing topic")
				continue
			}
			t := bus.getOrCreate(topic)
			if afterSeqRaw, ok := rawReq["after_seq"]; ok {
				afterSeq := uint64(afterSeqRaw.(float64))
				result := t.since(afterSeq)
				resp := map[string]interface{}{
					"ok":       true,
					"topic":    topic,
					"messages": result.Messages,
				}
				if result.Overflow {
					resp["overflow"] = true
					resp["oldest_seq"] = result.OldestSeq
				}
				enc.Encode(resp)
			} else {
				msg, ok := t.latest()
				if !ok {
					reply(response{OK: true, Topic: topic})
					continue
				}
				reply(response{
					OK: true, Topic: msg.Topic, Mime: msg.Mime,
					Seq: msg.Seq, Ts: msg.Timestamp,
					Data: msg.Data, Sender: msg.Sender,
				})
			}

		case "sub":
			subReq, err := ParseSubRequest(rawReq)
			if err != nil {
				fail(err.Error())
				continue
			}
			switch subReq.Mode {
			case SubExact:
				handleSubExact(conn, enc, bus, subReq.Topic)
			case SubPattern:
				handleSubPattern(conn, enc, bus, subReq.Pattern, connID)
			case SubMulti:
				handleSubMulti(conn, enc, bus, subReq.Topics)
			}
			return

		case "create_topic":
			topic, _ := rawReq["topic"].(string)
			if topic == "" {
				fail("create_topic requires topic")
				continue
			}
			size := 0
			if rs, ok := rawReq["ring_size"].(float64); ok {
				size = int(rs)
			}
			t, created := bus.createTopic(topic, size)
			enc.Encode(map[string]interface{}{
				"ok":        true,
				"topic":     topic,
				"ring_size": t.cap,
				"created":   created,
			})

		case "topics":
			reply(response{OK: true, Topics: bus.topicNames()})

		case "poll":
			topic, _ := rawReq["topic"].(string)
			if topic == "" {
				fail("missing topic")
				continue
			}
			afterSeq := uint64(0)
			if as, ok := rawReq["after_seq"].(float64); ok {
				afterSeq = uint64(as)
			}
			limit := 10
			if l, ok := rawReq["limit"].(float64); ok {
				limit = int(l)
			}
			timeoutMs := 2000
			if t, ok := rawReq["timeout_ms"].(float64); ok {
				timeoutMs = int(t)
			}
			t := bus.getOrCreate(topic)
			result := t.since(afterSeq)
			msgs := result.Messages
			if len(msgs) > 0 {
				if len(msgs) > limit {
					msgs = msgs[:limit]
				}
				reply(response{OK: true, Topic: topic, Messages: msgs})
				continue
			}
			id, ch := t.subscribe()
			timer := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
			var collected []Message
			done := false
			for !done {
				select {
				case msg := <-ch:
					if msg.Seq > afterSeq {
						collected = append(collected, msg)
						if len(collected) >= limit {
							done = true
						}
					}
				case <-timer.C:
					done = true
				}
			}
			timer.Stop()
			t.unsubscribe(id)
			reply(response{OK: true, Topic: topic, Messages: collected})

		case "manifest":
			reply(response{OK: true, Manifest: buildManifest(bus)})

		case "terminal_context":
			cmdTopic := bus.getOrCreate("terminal.command")
			outTopic := bus.getOrCreate("terminal.output")
			cmdMsg, cmdOk := cmdTopic.latest()
			outMsg, outOk := outTopic.latest()
			resp := map[string]interface{}{"ok": true}
			if cmdOk {
				resp["command"] = cmdMsg.Data
				resp["command_seq"] = cmdMsg.Seq
				resp["command_ts"] = cmdMsg.Timestamp
			} else {
				resp["command"] = ""
			}
			if outOk {
				resp["output"] = outMsg.Data
				resp["output_seq"] = outMsg.Seq
				resp["output_ts"] = outMsg.Timestamp
			} else {
				resp["output"] = ""
			}
			enc.Encode(resp)

		case "tail":
			topic, _ := rawReq["topic"].(string)
			if topic == "" {
				fail("tail requires topic")
				continue
			}
			n := 10
			if nv, ok := rawReq["n"].(float64); ok && nv > 0 {
				n = int(nv)
			}
			t := bus.getOrCreate(topic)
			result := t.since(0)
			msgs := result.Messages
			if len(msgs) > n {
				msgs = msgs[len(msgs)-n:]
			}
			enc.Encode(map[string]interface{}{"ok": true, "topic": topic, "messages": msgs})

		case "wait":
			topic, _ := rawReq["topic"].(string)
			if topic == "" {
				fail("wait requires topic")
				continue
			}
			timeoutMs := 30000
			if tv, ok := rawReq["timeout_ms"].(float64); ok && tv > 0 {
				timeoutMs = int(tv)
			}
			t := bus.getOrCreate(topic)
			// get current latest seq so we only return something newer
			var afterSeq uint64
			if latest, ok := t.latest(); ok {
				afterSeq = latest.Seq
			}
			id, ch := t.subscribe()
			timer := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
			var got *Message
			waitDone := false
			for !waitDone {
				select {
				case msg := <-ch:
					if msg.Seq > afterSeq {
						copy := msg
						got = &copy
						waitDone = true
					}
				case <-timer.C:
					waitDone = true
				}
			}
			timer.Stop()
			t.unsubscribe(id)
			if got == nil {
				reply(response{OK: false, Error: "timeout waiting for message on " + topic})
			} else {
				reply(response{OK: true, Topic: got.Topic, Mime: got.Mime, Seq: got.Seq, Ts: got.Timestamp, Data: got.Data, Sender: got.Sender})
			}

		case "copy":
			data, _ := rawReq["data"].(string)
			t := bus.getOrCreate("kernclip.clipboard")
			msg := Message{Topic: "kernclip.clipboard", Mime: "text/plain", Data: data, Sender: pid}
			msg = t.publish(msg)
			bus.patterns.FanOut("kernclip.clipboard", msg)
			reply(response{OK: true, Topic: "kernclip.clipboard", Seq: msg.Seq})

		case "paste":
			t := bus.getOrCreate("kernclip.clipboard")
			msg, ok := t.latest()
			if !ok {
				reply(response{OK: true, Topic: "kernclip.clipboard", Data: ""})
			} else {
				reply(response{OK: true, Topic: msg.Topic, Mime: msg.Mime, Seq: msg.Seq, Ts: msg.Timestamp, Data: msg.Data})
			}

		case "status":
			names := bus.topicNames()
			enc.Encode(map[string]interface{}{
				"ok":           true,
				"running":      true,
				"topic_count":  len(names),
				"topics":       names,
				"socket":       socketPath(),
				"version":      "1.1.0",
			})

				default:
			fail(fmt.Sprintf("unknown op: %q", op))
		}
	}
}

// handleSubExact streams messages for a single topic (original behavior).
func handleSubExact(conn net.Conn, enc *json.Encoder, bus *Bus, topic string) {
	t := bus.getOrCreate(topic)
	id, ch := t.subscribe()
	defer t.unsubscribe(id)

	for msg := range ch {
		if err := enc.Encode(response{
			OK: true, Topic: msg.Topic, Mime: msg.Mime,
			Seq: msg.Seq, Ts: msg.Timestamp,
			Data: msg.Data, Sender: msg.Sender,
		}); err != nil {
			return
		}
	}
}

// handleSubPattern streams messages matching a glob pattern.
func handleSubPattern(conn net.Conn, enc *json.Encoder, bus *Bus, pattern string, connID string) {
	compiled, err := CompilePattern(pattern)
	if err != nil {
		enc.Encode(response{OK: false, Error: err.Error()})
		return
	}

	id := fmt.Sprintf("%p", conn) // simple unique ID
	ch := make(chan Message, 256)

	psub := &PatternSub{
		ID:       id,
		Pattern:  pattern,
		Compiled: compiled,
		ConnID:   connID,
		Ch:       ch,
	}
	bus.patterns.Register(psub)
	defer bus.patterns.Unregister(id)
	defer close(ch)

	for msg := range ch {
		env := map[string]interface{}{
			"ok":      true,
			"seq":     msg.Seq,
			"topic":   msg.Topic,
			"pattern": pattern,
			"ts":      msg.Timestamp,
			"mime":    msg.Mime,
			"data":    msg.Data,
			"sender":  msg.Sender,
		}
		if err := enc.Encode(env); err != nil {
			return
		}
	}
}

// handleSubMulti streams messages from multiple topics (fan-in).
func handleSubMulti(conn net.Conn, enc *json.Encoder, bus *Bus, topics []string) {
	merged := make(chan Message, 512)
	var wg sync.WaitGroup

	for _, topic := range topics {
		wg.Add(1)
		go func(t string) {
			defer wg.Done()
			top := bus.getOrCreate(t)
			id, ch := top.subscribe()
			defer top.unsubscribe(id)
			for msg := range ch {
				select {
				case merged <- msg:
				default: // drop if merged buffer full
				}
			}
		}(topic)
	}

	go func() {
		wg.Wait()
		close(merged)
	}()

	for msg := range merged {
		if err := enc.Encode(response{
			OK: true, Topic: msg.Topic, Mime: msg.Mime,
			Seq: msg.Seq, Ts: msg.Timestamp,
			Data: msg.Data, Sender: msg.Sender,
		}); err != nil {
			return
		}
	}
}

// ── Main ──────────────────────────────────────────────────────────────────────

func socketPath() string {
	uid := os.Getuid()
	return fmt.Sprintf("/run/user/%d/kernclip-bus.sock", uid)
}

func main() {
	sockPath := socketPath()
	os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		log.Fatalf("listen %s: %v", sockPath, err)
	}
	defer os.Remove(sockPath)

	if err := os.Chmod(sockPath, 0600); err != nil {
		log.Printf("chmod socket: %v", err)
	}

	bus := newBus()

	log.Printf("kernclip-busd listening on %s", sockPath)

	// graceful shutdown
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("shutting down")
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return // closed
		}
		go handleConn(conn, bus)
	}
}
