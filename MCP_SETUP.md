# KernClip Bus — MCP Server Setup

## Claude Desktop config

Add to `~/.config/Claude/claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "kernclip-bus": {
      "command": "kernclip-bus-mcp",
      "args": []
    }
  }
}
```

**Prerequisite:** `kernclip-busd` must be running before Claude Desktop starts.

Add to your session startup (e.g. `~/.profile` or systemd user service):
```bash
kernclip-busd &
```

Or as a systemd user service:
```ini
# ~/.config/systemd/user/kernclip-busd.service
[Unit]
Description=KernClip Bus Daemon
After=default.target

[Service]
ExecStart=kernclip-busd
Restart=always

[Install]
WantedBy=default.target
```
```bash
systemctl --user enable --now kernclip-busd
```

---

## How the dynamic architecture works

```
Claude → tools/list  → MCP server → {"op":"manifest"} → busd
                                  ← op schemas (live)
                     ← generated tool defs (fresh every call)

Claude → tools/call bus_pub → MCP server → {"op":"pub",...} → busd
```

**The MCP server has zero hardcoded tools.**

To expose a new bus operation to Claude:
1. Add the op handler to `main.go` (the `switch req.Op` block)
2. Add its `OpDef` to `buildManifest()` in `main.go`
3. Rebuild `kernclip-busd`
4. Restart `kernclip-busd`

Claude sees the new tool on the next `tools/list` call. The MCP server binary
(`kernclip-bus-mcp`) never needs to be recompiled or restarted.

---

## Tools Claude sees (as of this build)

| Tool | Description |
|------|-------------|
| `bus_pub` | Publish data to any named topic |
| `bus_get` | Read latest message, or replay since a seq |
| `bus_poll` | Wait for new messages (MCP-safe alternative to sub) |
| `bus_topics` | List all active topics |
| `bus_manifest` | Inspect the bus's own self-description |

---

## Example: Claude ↔ external agent collaboration

Claude publishes a task:
```
bus_pub(topic="ai.session.001.input", data="Summarize Q4 trends", sender_name="claude")
```

External agent (any model, any language) subscribes, does its work, publishes back:
```python
for msg in bus.sub("ai.session.001.input"):
    result = call_my_model(msg.data)
    bus.pub("ai.session.001.output", result)
```

Claude polls for the result:
```
bus_poll(topic="ai.session.001.output", after_seq=0, timeout_ms=30000)
```

No shared memory. No SDK coupling. No broker. Just a Unix socket and JSON.

---

## Log file

MCP server logs to `/tmp/kernclip-mcp.log` for debugging.
