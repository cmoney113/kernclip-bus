#!/usr/bin/env python3
"""
shared-clipboard — Any process reads/writes the clipboard topic.
Shows cross-process clipboard sharing without Wayland/X11 involvement.
"""

import sys, os, time, threading
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '../../sdk/python'))
from kernclip_bus import Bus

bus = Bus()

def watcher():
    print("[Watcher] Monitoring clipboard changes...")
    for msg in bus.sub("kernclip.clipboard"):
        print(f"[Watcher] Clipboard updated by {msg.sender!r}: {msg.data!r}")

threading.Thread(target=watcher, daemon=True).start()
time.sleep(0.1)

for text in ["hello from process A", "now process B writes this", "done"]:
    bus.set_clipboard(text)
    print(f"[Writer] Set clipboard: {text!r}")
    time.sleep(0.2)

time.sleep(0.1)
print(f"\n[Reader] Current clipboard: {bus.get_clipboard()!r}")
