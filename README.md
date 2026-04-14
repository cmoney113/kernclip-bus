# KernClip Bus

A dead-simple, zero-broker, local IPC message bus. Any process publishes data to a named topic. Any other process reads it. That's the whole thing.

```
Entity A  ──pub──▶  kernclip-busd  ◀──sub──  Entity B
                    (Unix socket)
                    (SHM ring buf)
```

**Key properties:**
- Zero dependencies to use it — any language that can open a Unix socket participates
- Newline-delimited JSON wire protocol — `echo '{"op":"pub",...}' | nc -U $SOCK`
- Persistent ring buffer per topic — subscribers can replay history
- Monotonic sequence numbers — exactly-once delivery, reconnect-safe
- The clipboard is just `topic: "kernclip.clipboard"` — nothing special

---

## Start the daemon

```bash
cd ~/kernclip/bus
go build -o kernclip-busd .
./kernclip-busd
# Listening on /run/user/1000/kernclip-bus.sock
```

---

## Wire protocol

Four operations over a Unix socket (`/run/user/$UID/kernclip-bus.sock`).  
Every message is one newline-terminated JSON object.

### pub — publish

```json
{"op":"pub","topic":"my-topic","mime":"text/plain","data":"hello world"}
```
```json
{"ok":true,"topic":"my-topic","seq":0}
```

### get — read latest

```json
{"op":"get","topic":"my-topic"}
```
```json
{"ok":true,"topic":"my-topic","mime":"text/plain","seq":0,"ts":1735000000000,"data":"hello world","sender":"12345"}
```

Replay history with `after_seq`:
```json
{"op":"get","topic":"my-topic","after_seq":5}
```
```json
{"ok":true,"topic":"my-topic","messages":[...]}
```

### sub — streaming subscription

```json
{"op":"sub","topic":"my-topic"}
```
Connection stays open. One JSON line per new message as they arrive.

### topics — list all topics

```json
{"op":"topics"}
```
```json
{"ok":true,"topics":["kernclip.clipboard","my-topic"]}
```

---

## Quickstart by language

### Shell (no deps at all)

```bash
source sdk/shell/kernclip-bus.sh

kc_pub "greetings" "hello from shell"
kc_get "greetings"
kc_sub "greetings"        # streaming
kc_topics
```

### Python (stdlib only)

```python
from sdk.python.kernclip_bus import Bus

bus = Bus()
bus.pub("my-topic", "hello from python")
msg = bus.get("my-topic")
print(msg.data)

for msg in bus.sub("my-topic"):   # blocking iterator
    print(msg.seq, msg.data)
```

### Node.js (stdlib only)

```javascript
const { Bus } = require('./sdk/node/kernclip-bus');

const bus = new Bus();
await bus.pub('my-topic', 'hello from node');
const msg = await bus.get('my-topic');
console.log(msg.data);

const unsub = bus.sub('my-topic', (msg) => console.log(msg.data));
```

### Go (stdlib only)

```go
bus := kernclip_bus.New()
bus.Pub("my-topic", "hello from go", "text/plain")
msg, _ := bus.Get("my-topic")
fmt.Println(msg.Data)

ctx, cancel := context.WithCancel(context.Background())
bus.Sub(ctx, "my-topic", func(msg kernclip_bus.Message) {
    fmt.Println(msg.Data)
})
```

### Raw netcat

```bash
echo '{"op":"pub","topic":"test","data":"raw"}' | nc -U /run/user/$UID/kernclip-bus.sock
echo '{"op":"get","topic":"test"}' | nc -U /run/user/$UID/kernclip-bus.sock
```

---

## AI model collaboration

The bus is model-agnostic and SDK-agnostic. Two AI processes coordinate by agreeing on topic names — nothing else needed.

```
Topic convention:
  ai.session.<id>.input    — tasks posted by orchestrator
  ai.session.<id>.output   — responses from any agent
  ai.session.<id>.memory   — shared working memory
  ai.session.<id>.done     — session complete
```

Agent A (Claude via Anthropic SDK) and Agent B (Gemini, Qwen, local Ollama — anything) both just connect to the same socket. They don't know about each other's SDKs, runtimes, or languages.

```python
# Agent A (Claude) — in one process
for task in bus.sub("ai.session.demo.input"):
    result = call_claude_api(task.data)
    bus.pub("ai.session.demo.output", result)

# Agent B (Gemini) — in a completely separate process
for result in bus.sub("ai.session.demo.output"):
    refined = call_gemini_api(result.data)
    bus.pub("ai.session.demo.done", refined)
```

Run the full demo: `python3 examples/ai-collab/demo.py`

---

## Examples

| Example | Description |
|---|---|
| `examples/ai-collab/` | Two AI models collaborating on a shared task |
| `examples/sensor-pipeline/` | Producer/consumer data pipeline |
| `examples/shared-clipboard/` | Cross-process clipboard via bus topic |

---

## Built-in topics

| Topic | Description |
|---|---|
| `kernclip.clipboard` | System clipboard (text/plain) |

---

## System monitor topics

Published automatically by `kernclip-busd` every 2 seconds — no configuration needed.

| Topic | Description |
|---|---|
| `gtt.system.metrics` | JSON: `cpu_percent`, `ram_used_percent`, `swap_used_percent`, `disk_read_speed`, `disk_write_speed`, `net_download_speed`, `net_upload_speed`, `process_count` |
| `gtt.system.top_disk_writers` | JSON array of top 5 processes by write speed: `{pid, name, write_speed}` |

```bash
# Live CPU % in your terminal:
kb sub gtt.system.metrics --data-only | jq .cpu_percent

# Watch disk writers:
kb sub gtt.system.top_disk_writers --data-only | jq .
```

---

## Command topics

Publish to a command topic → the daemon executes a side effect and ACKs on `gtt.system.ack.*`.

| Publish to | Data format | Effect |
|---|---|---|
| `gtt.settings.brightness` | `"50"` | Set screen brightness 0–100% (brightnessctl / xrandr) |
| `gtt.settings.volume` | `"40"` | Set audio volume 0–150% (wpctl → pactl → amixer) |
| `gtt.settings.nightlight` | `"true"` or `"false"` | Toggle GNOME Night Light via gsettings |
| `gtt.settings.dconf` | `"/path/key value"` | `dconf write` |
| `gtt.settings.gsettings` | `"schema key value"` | `gsettings set` |
| `gtt.system.kill` | `"1234"` | `kill -9 <pid>` |
| `gtt.system.killname` | `"chrome"` | `pkill -9 <name>` |
| `gtt.system.service` | `"restart:nginx"` | `systemctl <action> <service>` (allow-listed actions only) |
| `gtt.system.notify` | `"Title\|Body"` | `notify-send` desktop notification |
| `gtt.system.exec` | `"bash cmd"` | Execute arbitrary shell command |

```bash
# Dim the screen to 30%:
kb pub gtt.settings.brightness 30

# Set volume to 50% and wait for confirmation:
kb pub gtt.settings.volume 50
kb wait gtt.system.ack.settings_volume --data-only

# Restart a systemd service:
kb pub gtt.system.service "restart:nginx"

# Send a desktop notification:
kb pub gtt.system.notify "Alert|CPU is above 90%"

# Write a dconf key:
kb pub gtt.settings.dconf "/org/gnome/desktop/interface/color-scheme 'prefer-dark'"
```

---

## Message envelope

Every message stored in the ring buffer carries:

```
seq       uint64   monotonic sequence number per topic
ts        int64    unix milliseconds
topic     string   topic name
mime      string   MIME type of data (default: text/plain)
sender    string   publisher process ID
data      string   payload (base64-encode binary, set mime accordingly)
```

---

## Ring buffer

Each topic has a 128-slot ring buffer (configurable). When full, oldest messages are overwritten. Subscribers that can't keep up have messages dropped rather than blocking the publisher.

Replay: `after_seq: N` returns all stored messages with seq > N, up to ring capacity.
