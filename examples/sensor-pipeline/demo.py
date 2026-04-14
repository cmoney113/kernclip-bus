#!/usr/bin/env python3
"""
sensor-pipeline — Any process publishes data, any process consumes it.

Simulates: IoT sensor → KernClip bus → data processor → dashboard

Run producer in one terminal, consumer in another. Or run this file.
"""

import sys, os
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '../../sdk/python'))

from kernclip_bus import Bus
import threading, time, json, random

def producer(bus: Bus, count: int = 10):
    """Simulates a sensor publishing readings."""
    print("[Producer] Publishing sensor data...")
    for i in range(count):
        reading = {
            "sensor_id": "temp-01",
            "value": round(20.0 + random.gauss(0, 2), 2),
            "unit": "celsius",
            "sample": i,
        }
        seq = bus.pub("sensors.temperature", json.dumps(reading), mime="application/json")
        print(f"[Producer] Published reading #{i} → seq={seq}")
        time.sleep(0.1)
    bus.pub("sensors.temperature.done", "eof")


def consumer(bus: Bus):
    """Consumes sensor data and computes a running average."""
    print("[Consumer] Subscribing to sensor data...")
    readings = []
    for msg in bus.sub("sensors.temperature"):
        if msg.mime == "application/json":
            data = json.loads(msg.data)
            readings.append(data["value"])
            avg = sum(readings) / len(readings)
            print(f"[Consumer] seq={msg.seq} value={data['value']}°C  avg={avg:.2f}°C  n={len(readings)}")
        # check if producer is done
        done = bus.get("sensors.temperature.done")
        if done and done.data == "eof":
            print(f"[Consumer] Stream complete. Final avg: {sum(readings)/len(readings):.2f}°C")
            break


if __name__ == "__main__":
    bus = Bus()
    c = threading.Thread(target=consumer, args=(bus,), daemon=True)
    c.start()
    time.sleep(0.1)
    producer(bus)
    c.join(timeout=5)
