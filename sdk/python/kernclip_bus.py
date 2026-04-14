"""
kernclip_bus.py — KernClip Bus SDK for Python
Zero dependencies. stdlib only.

Usage:
    from kernclip_bus import Bus

    bus = Bus()
    bus.pub("my-topic", "hello world")
    msg = bus.get("my-topic")
    for msg in bus.sub("my-topic"):   # blocking iterator
        print(msg)
"""

import json
import os
import base64
import mimetypes
import socket
import threading
from pathlib import Path
from typing import Callable, Iterator, List, Optional, Union


def _default_socket() -> str:
    return f"/run/user/{os.getuid()}/kernclip-bus.sock"


class BusError(Exception):
    pass


class Message:
    __slots__ = ("topic", "mime", "data", "seq", "ts", "sender", "pattern")

    def __init__(self, d: dict):
        self.topic  = d.get("topic", "")
        self.mime   = d.get("mime", "text/plain")
        self.data   = d.get("data", "")
        self.seq    = d.get("seq", 0)
        self.ts     = d.get("ts", 0)
        self.sender = d.get("sender", "")
        self.pattern = d.get("pattern")  # set when message came from pattern sub

    def __repr__(self):
        if self.pattern:
            return f"Message(topic={self.topic!r}, pattern={self.pattern!r}, seq={self.seq}, data={self.data!r})"
        return f"Message(topic={self.topic!r}, seq={self.seq}, data={self.data!r})"


class Bus:
    def __init__(self, socket_path: Optional[str] = None):
        self.socket_path = socket_path or _default_socket()

    def _connect(self) -> socket.socket:
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            s.connect(self.socket_path)
        except FileNotFoundError:
            raise BusError(
                f"kernclip-busd not running. Start it with: kernclip-busd\n"
                f"Socket: {self.socket_path}"
            )
        return s

    def _roundtrip(self, msg: dict) -> dict:
        with self._connect() as s:
            s.sendall((json.dumps(msg) + "\n").encode())
            data = b""
            while True:
                chunk = s.recv(65536)
                if not chunk:
                    break
                data += chunk
                if b"\n" in data:
                    break
            return json.loads(data.split(b"\n")[0])

    def _readlines(self, conn: socket.socket):
        """Read newline-delimited lines from a streaming connection."""
        buf = b""
        while True:
            chunk = conn.recv(65536)
            if not chunk:
                break
            buf += chunk
            while b"\n" in buf:
                line, buf = buf.split(b"\n", 1)
                if line.strip():
                    yield line

    def _parse_message(self, raw: bytes) -> Message:
        """Parse a raw JSON line into a Message."""
        d = json.loads(raw)
        return Message(d)

    def pub(self, topic: str, data: str, mime: str = "text/plain") -> int:
        """Publish data to a topic. Returns the assigned seq number."""
        r = self._roundtrip({"op": "pub", "topic": topic, "mime": mime, "data": data})
        if not r.get("ok"):
            raise BusError(r.get("error", "unknown error"))
        return r.get("seq", 0)

    def get(self, topic: str, after_seq: Optional[int] = None) -> Union[Message, List[Message], dict, None]:
        """
        Get the latest message on a topic, or None if empty.
        Pass after_seq to fetch all messages newer than that seq.
        
        When after_seq is used, returns a dict with:
          - messages: list of Message objects
          - overflow: bool (true if some history was dropped)
          - oldest_seq: oldest seq currently in ring
        """
        req = {"op": "get", "topic": topic}
        if after_seq is not None:
            req["after_seq"] = after_seq
        r = self._roundtrip(req)
        if not r.get("ok"):
            raise BusError(r.get("error", "unknown error"))
        if after_seq is not None:
            # Return full response as dict for overflow checking
            result = {
                "messages": [Message(m) for m in r.get("messages", [])],
                "overflow": r.get("overflow", False),
                "oldest_seq": r.get("oldest_seq", 0),
            }
            return result
        if not r.get("data") and not r.get("seq"):
            return None
        return Message(r)

    def sub(self, topic: str = None, *, pattern: str = None, topics: List[str] = None) -> Iterator[Message]:
        """
        Subscribe to topics. Blocking iterator — yields messages as they arrive.
        
        Three modes:
        
        Exact (original):
            for msg in bus.sub("my-topic"):
                print(msg)
        
        Pattern (wildcard):
            for msg in bus.sub(pattern="agent.*.file_written"):
                print(msg.topic, msg.pattern)  # pattern shows which subscription fired
        
        Multi-topic (fan-in):
            for msg in bus.sub(topics=["agent.a.thought", "agent.b.thought"]):
                print(msg.topic, msg.data)
        """
        # Type-check to catch "passed function instead of string" bugs early
        def check_str(name: str, val) -> str:
            if val is None:
                return None
            if callable(val):
                raise TypeError(
                    f"sub() {name}= must be str, not a function. "
                    f"Did you pass callback= instead of pattern=?"
                )
            return str(val)

        pattern = check_str("pattern", pattern)
        topic = check_str("topic", topic)

        if pattern is not None:
            op = {"op": "sub", "pattern": pattern}
        elif topics is not None:
            op = {"op": "sub", "topics": topics}
        elif topic is not None:
            op = {"op": "sub", "topic": topic}
        else:
            raise ValueError("sub() requires topic, pattern, or topics")

        conn = self._connect()
        try:
            conn.sendall((json.dumps(op) + "\n").encode())
            for line in self._readlines(conn):
                yield self._parse_message(line)
        finally:
            conn.close()

    def sub_async(self, topic: str, callback, *, daemon: bool = True) -> threading.Thread:
        """
        Subscribe in a background thread, calling callback(msg) for each message.

        Example:
            def on_msg(msg):
                print("got:", msg.data)
            bus.sub_async("ai.responses", on_msg)

        Returns the thread (caller can join to wait).
        """
        # Validate: topic must be str, not function
        if callable(topic):
            raise TypeError("sub_async topic= must be str, not a function")

        def _run():
            for msg in self.sub(topic):
                try:
                    callback(msg)
                except Exception:
                    pass

        t = threading.Thread(target=_run, daemon=daemon)
        t.start()
        return t

    def create_topic(self, topic: str, ring_size: Optional[int] = None) -> dict:
        """
        Explicitly create a topic with optional ring size override.
        Useful for agent topics before the agent starts publishing.
        
        Returns dict with: ok, topic, ring_size, created (bool)
        
        Example:
            bus.create_topic("agent.abc123.thought", ring_size=4096)
        """
        op = {"op": "create_topic", "topic": topic}
        if ring_size is not None:
            op["ring_size"] = ring_size
        return self._roundtrip(op)

    def manifest(self) -> dict:
        """
        Get self-describing schema of the daemon.
        Returns: version, socket, ops, topics with stats.
        """
        return self._roundtrip({"op": "manifest"})

    def topics(self) -> list:
        """List all active topic names."""
        r = self._roundtrip({"op": "topics"})
        return r.get("topics", [])

    # Convenience: clipboard shorthand
    def get_clipboard(self) -> Optional[str]:
        msg = self.get("kernclip.clipboard")
        return msg.data if msg else None

    def set_clipboard(self, text: str) -> None:
        self.pub("kernclip.clipboard", text, mime="text/plain")

    # ── Binary / file helpers ──────────────────────────────────────────────────

    def pub_bytes(self, topic: str, data: bytes, filename: str = "data.bin",
                  mime: Optional[str] = None) -> int:
        """Publish raw bytes as a base64 file envelope."""
        if mime is None:
            guessed, _ = mimetypes.guess_type(filename)
            mime = guessed or "application/octet-stream"
        envelope = json.dumps({
            "encoding": "base64",
            "filename": filename,
            "size": len(data),
            "data": base64.b64encode(data).decode(),
        })
        return self.pub(topic, envelope, mime=mime)

    def pub_file(self, topic: str, path: Union[str, Path],
                 mime: Optional[str] = None) -> int:
        """Read a file from disk and publish it as a base64 envelope."""
        p = Path(path)
        raw = p.read_bytes()
        if mime is None:
            guessed, _ = mimetypes.guess_type(p.name)
            mime = guessed or "application/octet-stream"
        return self.pub_bytes(topic, raw, filename=p.name, mime=mime)

    def get_bytes(self, topic: str) -> Optional[tuple]:
        """
        Get the latest message and attempt to decode it as a file envelope.
        Returns (raw_bytes, filename, mime) or None if empty or not a file payload.
        """
        msg = self.get(topic)
        if msg is None:
            return None
        return _decode_file_envelope(msg)

    def get_file(self, topic: str, dest: Union[str, Path, None] = None
                 ) -> Optional[Path]:
        """
        Get the latest file payload from a topic and save it to disk.
        If dest is a directory (or None → cwd), the original filename is used.
        Returns the Path where the file was saved, or None if no file payload.
        """
        result = self.get_bytes(topic)
        if result is None:
            return None
        raw, filename, _ = result
        out = Path(dest) if dest else Path(filename)
        if out.is_dir():
            out = out / filename
        out.write_bytes(raw)
        return out


def _decode_file_envelope(msg: Message) -> Optional[tuple]:
    """Parse a base64 file envelope from a Message. Returns None if not one."""
    try:
        env = json.loads(msg.data)
    except (json.JSONDecodeError, TypeError):
        return None
    if env.get("encoding") != "base64" or not env.get("data"):
        return None
    try:
        raw = base64.b64decode(env["data"])
    except Exception:
        return None
    return raw, env.get("filename", "data.bin"), msg.mime
