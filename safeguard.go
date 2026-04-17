package main

import (
	"log"
	"runtime"
	"runtime/debug"
	"time"
)

// InitSafeguards prevents AI-generated footguns
func InitSafeguards() {
	// 1. Limit max goroutines
	debug.SetMaxThreads(1000)

	// 2. GC tuning for high-throughput
	debug.SetGCPercent(50)

	// 3. Goroutine leak detector
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if runtime.NumGoroutine() > 500 {
				log.Printf("[SAFETY] Goroutine leak detected: %d goroutines", runtime.NumGoroutine())
				// Force GC and stack dump
				debug.FreeOSMemory()
				buf := make([]byte, 1<<20)
				runtime.Stack(buf, true)
				log.Printf("[SAFETY] Stack dump:\n%s", buf)
			}
		}
	}()
}
