#!/usr/bin/env bash
# kernclip-bus.sh — KernClip Bus SDK for shell scripts
# Source this file, then use: kc_pub, kc_get, kc_sub, kc_topics
#
# Usage:
#   source kernclip-bus.sh
#   kc_pub "my-topic" "hello world"
#   kc_get "my-topic"
#   kc_sub "my-topic"   # streams, one JSON line per message

KC_SOCK="${KC_SOCK:-/run/user/$(id -u)/kernclip-bus.sock}"

_kc_send() {
    echo "$1" | socat - UNIX-CONNECT:"$KC_SOCK" 2>/dev/null \
        || echo "$1" | nc -U "$KC_SOCK" 2>/dev/null
}

# Publish data to a topic
# Usage: kc_pub <topic> <data> [mime]
kc_pub() {
    local topic="$1" data="$2" mime="${3:-text/plain}"
    _kc_send "{\"op\":\"pub\",\"topic\":\"$topic\",\"mime\":\"$mime\",\"data\":$(printf '%s' "$data" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')}"
}

# Get latest message from a topic (prints data field)
# Usage: kc_get <topic>
kc_get() {
    local topic="$1"
    local resp
    resp=$(_kc_send "{\"op\":\"get\",\"topic\":\"$topic\"}")
    echo "$resp" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("data",""))' 2>/dev/null \
        || echo "$resp"
}

# Get raw JSON response for a topic
kc_get_json() {
    _kc_send "{\"op\":\"get\",\"topic\":\"$1\"}"
}

# Subscribe to a topic — streams one JSON line per message
# Ctrl-C to stop
# Usage: kc_sub <topic>
kc_sub() {
    local topic="$1"
    python3 - "$topic" << 'EOF'
import sys, json, socket, os
topic = sys.argv[1]
sock_path = f"/run/user/{os.getuid()}/kernclip-bus.sock"
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.connect(sock_path)
s.sendall((json.dumps({"op": "sub", "topic": topic})+'\n').encode())
while True:
    line = s.recv(65536)
    if not line: break
    sys.stdout.write(line.decode())
    sys.stdout.flush()
EOF
}

# Subscribe and print only the data field
# Usage: kc_sub_data <topic>
kc_sub_data() {
    kc_sub "$1" | python3 -c '
import json, sys
for line in sys.stdin:
    line = line.strip()
    if line:
        try:
            d = json.loads(line)
            print(d.get("data", ""), flush=True)
        except: pass
'
}

# List all topics
kc_topics() {
    _kc_send '{"op":"topics"}' \
        | python3 -c 'import json,sys; d=json.load(sys.stdin); [print(t) for t in d.get("topics",[])]'
}


# ── New subscription modes ─────────────────────────────────────────────────────

# Subscribe with wildcard pattern
# Usage: kc_sub_pattern "agent.*"
kc_sub_pattern() {
    local pattern="$1"
    python3 - "$pattern" << 'EOF'
import sys, json, socket, os
pattern = sys.argv[1]
sock_path = f"/run/user/{os.getuid()}/kernclip-bus.sock"
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.connect(sock_path)
s.sendall((json.dumps({"op": "sub", "pattern": pattern})+'\n').encode())
while True:
    line = s.recv(65536)
    if not line: break
    sys.stdout.write(line.decode())
    sys.stdout.flush()
EOF
}

# Subscribe to multiple topics (fan-in)
# Usage: kc_sub_multi "a,b,c"
kc_sub_multi() {
    local topics="$1"
    python3 - "$topics" << 'EOF'
import sys, json, socket, os
topics = sys.argv[1].split(',')
sock_path = f"/run/user/{os.getuid()}/kernclip-bus.sock"
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.connect(sock_path)
s.sendall((json.dumps({'op':'sub','topics':topics})+'\n').encode())
while True:
    line = s.recv(65536)
    if not line: break
    sys.stdout.write(line.decode())
    sys.stdout.flush()
EOF
}

# ── Topic management ───────────────────────────────────────────────────────────

# Create a topic with optional ring size
# Usage: kc_create_topic <topic> [ring_size]
kc_create_topic() {
    local topic="$1" ring_size="${2:-}"
    python3 - "$topic" "${ring_size:-}" << 'EOF'
import sys, json, socket, os
topic, ring_size = sys.argv[1], sys.argv[2]
req = {"op": "create_topic", "topic": topic}
if ring_size:
    req["ring_size"] = int(ring_size)
sock_path = f"/run/user/{os.getuid()}/kernclip-bus.sock"
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.connect(sock_path)
s.sendall((json.dumps(req)+'\n').encode())
resp = json.loads(s.recv(4096).split(b'\n')[0])
s.close()
print(f"✓ {resp['topic']} ring_size={resp['ring_size']} created={resp['created']}")
EOF
}

# Get daemon manifest (self-describing schema)
# Usage: kc_manifest
kc_manifest() {
    python3 - << 'EOF'
import json, socket, os
sock_path = f"/run/user/{os.getuid()}/kernclip-bus.sock"
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.connect(sock_path)
s.sendall((json.dumps({"op": "manifest"})+'\n').encode())
resp = json.loads(s.recv(65536).split(b'\n')[0])
s.close()
print(json.dumps(resp, indent=2))
EOF
}


# Clipboard shortcuts
kc_copy()  { kc_pub "kernclip.clipboard" "$1"; }
kc_paste() { kc_get "kernclip.clipboard"; }

# Paste clipboard contents by typing them into the active window via RD bus portal
# Uses gtt (gttd daemon) for true keyboard injection — works on ANY surface
# Usage: kc_paste_type
kc_paste_type() {
    local text
    text=$(kc_paste)
    if [[ -n "$text" ]]; then
        gtt --type "$text"
    else
        echo "(clipboard empty)" >&2
        return 1
    fi
}

# ── File helpers ──────────────────────────────────────────────────────────────

# Publish a file to a topic as a base64 envelope
# Usage: kc_pub_file <topic> <filepath> [mime]
kc_pub_file() {
    local topic="$1" fpath="$2"
    if [[ -z "$fpath" || ! -f "$fpath" ]]; then
        echo "kc_pub_file: file not found: $fpath" >&2
        return 1
    fi
    python3 - "$topic" "$fpath" "${3:-}" << 'EOF'
import sys, json, os, base64, mimetypes
topic, fpath = sys.argv[1], sys.argv[2]
mime = sys.argv[3] if len(sys.argv) > 3 and sys.argv[3] else None
raw = open(fpath, 'rb').read()
if not mime:
    mime, _ = mimetypes.guess_type(fpath)
    mime = mime or 'application/octet-stream'
envelope = json.dumps({
    'encoding': 'base64',
    'filename': os.path.basename(fpath),
    'size': len(raw),
    'data': base64.b64encode(raw).decode(),
})
import socket
sock_path = f"/run/user/{os.getuid()}/kernclip-bus.sock"
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.connect(sock_path)
s.sendall((json.dumps({'op':'pub','topic':topic,'mime':mime,'data':envelope})+'\n').encode())
resp = json.loads(s.recv(4096).split(b'\n')[0])
s.close()
print(f"✓ {topic}  {mime}  {len(raw)} bytes → base64  seq={resp.get('seq',0)}")
EOF
}

# Get a file payload from a topic and save it to disk
# Usage: kc_get_file <topic> [dest_dir_or_path]
kc_get_file() {
    local topic="$1" dest="${2:-.}"
    python3 - "$topic" "$dest" << 'EOF'
import sys, json, os, base64
topic, dest = sys.argv[1], sys.argv[2]
import socket
sock_path = f"/run/user/{os.getuid()}/kernclip-bus.sock"
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.connect(sock_path)
s.sendall((json.dumps({'op':'get','topic':topic})+'\n').encode())
resp = json.loads(s.recv(65536).split(b'\n')[0])
s.close()
data = resp.get('data','')
if not data:
    print('(empty)', file=sys.stderr); sys.exit(0)
try:
    env = json.loads(data)
    assert env.get('encoding') == 'base64'
    raw = base64.b64decode(env['data'])
    fname = env.get('filename','data.bin')
    out = os.path.join(dest, fname) if os.path.isdir(dest) else dest
    open(out,'wb').write(raw)
    print(f"✓ saved → {out}  ({len(raw)} bytes)")
except Exception:
    print(data)
EOF
}

echo "KernClip Bus loaded. Socket: $KC_SOCK" >&2
