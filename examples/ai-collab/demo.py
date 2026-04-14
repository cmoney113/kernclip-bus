#!/usr/bin/env python3
"""
ai-collab/README example — Two AI models collaborating over the KernClip bus.

This demonstrates the core concept: any AI process, any model, any SDK
can participate in a shared reasoning session via named topics.

Run agent_a.py in one terminal, agent_b.py in another.
Or run this file which simulates both in separate threads.
"""

import sys
import os
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '../../sdk/python'))

from kernclip_bus import Bus
import threading
import time

# ── Topic convention ──────────────────────────────────────────────────────────
#
# ai.session.<session_id>.input    — prompts / tasks posted by orchestrator
# ai.session.<session_id>.output   — responses from any agent
# ai.session.<session_id>.memory   — shared working memory (any agent can write)
# ai.session.<session_id>.done     — posted when session is complete
#
# Any model (Claude, Gemini, Qwen, local Ollama, whatever) just needs to:
#   1. sub to ai.session.<id>.input
#   2. pub responses to ai.session.<id>.output
# That's the entire contract.

SESSION = "demo-001"

def agent_a(bus: Bus):
    """
    Simulates Agent A (e.g. Claude via API).
    Listens for tasks, does its part, posts result.
    """
    topic_in  = f"ai.session.{SESSION}.input"
    topic_out = f"ai.session.{SESSION}.output"
    topic_mem = f"ai.session.{SESSION}.memory"

    print(f"[Agent A] Listening on {topic_in}")
    for msg in bus.sub(topic_in):
        task = msg.data
        print(f"[Agent A] Received task: {task!r}")

        if task == "STOP":
            break

        # Simulate doing work (in real life: call Anthropic API here)
        result = f"Agent A processed: '{task}' → result_A"
        time.sleep(0.1)

        # Write to shared memory so Agent B can build on it
        bus.pub(topic_mem, result, mime="text/plain")
        print(f"[Agent A] Wrote to memory: {result!r}")

        # Publish response
        bus.pub(topic_out, result, mime="text/plain")
        print(f"[Agent A] Published output")


def agent_b(bus: Bus):
    """
    Simulates Agent B (e.g. Gemini, Qwen, local model — doesn't matter).
    Watches shared memory and output, builds on Agent A's work.
    """
    topic_out = f"ai.session.{SESSION}.output"
    topic_mem = f"ai.session.{SESSION}.memory"

    print(f"[Agent B] Watching {topic_out}")
    for msg in bus.sub(topic_out):
        print(f"[Agent B] Saw output: {msg.data!r}")

        # Read shared memory to see what Agent A stored
        mem = bus.get(topic_mem)
        if mem:
            print(f"[Agent B] Read memory: {mem.data!r}")

        # Build on Agent A's work
        followup = f"Agent B refined: [{msg.data}] → final_result"
        bus.pub(f"ai.session.{SESSION}.done", followup)
        print(f"[Agent B] Published final result: {followup!r}")
        break  # one round for demo


def orchestrator(bus: Bus):
    """Posts tasks to the session."""
    time.sleep(0.3)  # let agents subscribe first
    topic_in = f"ai.session.{SESSION}.input"
    print(f"\n[Orchestrator] Posting task to {topic_in}")
    bus.pub(topic_in, "Analyze the market trends for Q4 2025")
    time.sleep(1.0)
    bus.pub(topic_in, "STOP")


def main():
    bus = Bus()

    try:
        bus.topics()  # probe connection
    except Exception as e:
        print(f"ERROR: {e}")
        sys.exit(1)

    # In real usage these would be separate processes / separate machines
    # Here we run them in threads to demo the protocol
    threads = [
        threading.Thread(target=agent_a, args=(bus,), daemon=True),
        threading.Thread(target=agent_b, args=(bus,), daemon=True),
        threading.Thread(target=orchestrator, args=(bus,), daemon=True),
    ]

    for t in threads:
        t.start()

    # Wait for final result
    print("\n[Main] Waiting for session result...")
    for msg in bus.sub(f"ai.session.{SESSION}.done"):
        print(f"\n[Main] FINAL RESULT: {msg.data}")
        break

    print("\nDone. All agents communicated via KernClip bus.")


if __name__ == "__main__":
    main()
