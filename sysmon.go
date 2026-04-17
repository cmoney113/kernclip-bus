package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── SystemMetrics snapshot ────────────────────────────────────────────────────

type SystemMetrics struct {
	Type             string             `json:"type"`
	Timestamp        float64            `json:"timestamp"`
	CPUPercent       float64            `json:"cpu_percent"`
	PerCoreCPU       []float64          `json:"per_core_cpu,omitempty"`
	RAMUsedPercent   float64            `json:"ram_used_percent"`
	RAMUsed          uint64             `json:"ram_used"`
	RAMTotal         uint64             `json:"ram_total"`
	SwapUsedPercent  float64            `json:"swap_used_percent"`
	DiskReadSpeed    float64            `json:"disk_read_speed"`
	DiskWriteSpeed   float64            `json:"disk_write_speed"`
	NetDownloadSpeed float64            `json:"net_download_speed"`
	NetUploadSpeed   float64            `json:"net_upload_speed"`
	ProcessCount     int                `json:"process_count"`
}

type DiskWriter struct {
	PID        int    `json:"pid"`
	Name       string `json:"name"`
	WriteSpeed float64 `json:"write_speed"` // bytes/s
}

// ── /proc CPU parsing ─────────────────────────────────────────────────────────

type cpuSample struct {
	total uint64
	idle  uint64
}

func parseCPUStat() ([]cpuSample, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil, err
	}
	var samples []cpuSample
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		var vals [10]uint64
		for i := 1; i < len(fields) && i <= 10; i++ {
			vals[i-1], _ = strconv.ParseUint(fields[i], 10, 64)
		}
		// user nice system idle iowait irq softirq steal guest guest_nice
		idle := vals[3] + vals[4] // idle + iowait
		total := vals[0] + vals[1] + vals[2] + vals[3] + vals[4] +
			vals[5] + vals[6] + vals[7]
		samples = append(samples, cpuSample{total: total, idle: idle})
	}
	return samples, nil
}

func calcCPUPercent(prev, curr []cpuSample) (overall float64, perCore []float64) {
	for i, c := range curr {
		if i >= len(prev) {
			break
		}
		totalDelta := float64(c.total - prev[i].total)
		idleDelta := float64(c.idle - prev[i].idle)
		if totalDelta == 0 {
			continue
		}
		pct := 100.0 * (1.0 - idleDelta/totalDelta)
		if i == 0 {
			overall = pct
		} else {
			perCore = append(perCore, pct)
		}
	}
	return
}

// ── /proc/meminfo ─────────────────────────────────────────────────────────────

func parseMemInfo() (ramUsed, ramTotal, swapUsed, swapTotal uint64, err error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return
	}
	var memTotal, memFree, memBuffers, memCached, shmem uint64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64)
		v *= 1024 // kB → bytes
		switch fields[0] {
		case "MemTotal:":
			memTotal = v
		case "MemFree:":
			memFree = v
		case "Buffers:":
			memBuffers = v
		case "Cached:":
			memCached = v
		case "Shmem:":
			shmem = v
		case "SwapTotal:":
			swapTotal = v
		case "SwapFree:":
			swapFree := v
			swapUsed = swapTotal - swapFree
		}
	}
	_ = shmem
	ramTotal = memTotal
	ramUsed = memTotal - memFree - memBuffers - memCached
	return
}

// ── /proc/diskstats ───────────────────────────────────────────────────────────

type diskSample struct {
	sectorsRead    uint64
	sectorsWritten uint64
}

const sectorSize = 512

func parseDiskStats() (map[string]diskSample, error) {
	data, err := os.ReadFile("/proc/diskstats")
	if err != nil {
		return nil, err
	}
	result := make(map[string]diskSample)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 14 {
			continue
		}
		name := fields[2]
		// skip partitions (e.g. sda1), keep whole disks (sda, nvme0n1)
		if len(name) > 3 && (name[len(name)-1] >= '0' && name[len(name)-1] <= '9') {
			// crude heuristic: skip if ends in digit and has a parent
			if strings.HasPrefix(name, "sd") || strings.HasPrefix(name, "hd") {
				continue
			}
		}
		sectorsRead, _ := strconv.ParseUint(fields[5], 10, 64)
		sectorsWritten, _ := strconv.ParseUint(fields[9], 10, 64)
		result[name] = diskSample{sectorsRead: sectorsRead, sectorsWritten: sectorsWritten}
	}
	return result, nil
}

func calcDiskSpeed(prev, curr map[string]diskSample, elapsed float64) (readSpeed, writeSpeed float64) {
	for name, c := range curr {
		p, ok := prev[name]
		if !ok {
			continue
		}
		readSpeed += float64(c.sectorsRead-p.sectorsRead) * sectorSize / elapsed
		writeSpeed += float64(c.sectorsWritten-p.sectorsWritten) * sectorSize / elapsed
	}
	return
}

// ── /proc/net/dev ─────────────────────────────────────────────────────────────

type netSample struct {
	rxBytes uint64
	txBytes uint64
}

func parseNetDev() (map[string]netSample, error) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	result := make(map[string]netSample)
	lines := strings.Split(string(data), "\n")
	for _, line := range lines[2:] { // skip 2 header lines
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		if name == "lo" {
			continue // skip loopback
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(fields[0], 10, 64)
		tx, _ := strconv.ParseUint(fields[8], 10, 64)
		result[name] = netSample{rxBytes: rx, txBytes: tx}
	}
	return result, nil
}

func calcNetSpeed(prev, curr map[string]netSample, elapsed float64) (dl, ul float64) {
	for name, c := range curr {
		p, ok := prev[name]
		if !ok {
			continue
		}
		dl += float64(c.rxBytes-p.rxBytes) / elapsed
		ul += float64(c.txBytes-p.txBytes) / elapsed
	}
	return
}

// ── Process count + top disk writers ─────────────────────────────────────────

func countProcesses() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() {
			if _, err := strconv.Atoi(e.Name()); err == nil {
				count++
			}
		}
	}
	return count
}

type pidIOSample struct {
	writeBytes uint64
}

func parseProcIO(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/io", pid))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "write_bytes:") {
			v, _ := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "write_bytes:")), 10, 64)
			return v, nil
		}
	}
	return 0, nil
}

func parseProcName(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(data))
}

const maxProcsToAnalyze = 50

func getTopDiskWriters(prevIO map[int]uint64, elapsed float64) ([]DiskWriter, map[int]uint64) {
	entries, _ := os.ReadDir("/proc")
	currIO := make(map[int]uint64)
	type pidWrite struct {
		pid   int
		speed float64
		name  string
	}
	var writers []pidWrite

	analyzed := 0
	for _, e := range entries {
		if analyzed >= maxProcsToAnalyze {
			break
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		writeBytes, err := parseProcIO(pid)
		if err != nil {
			continue
		}
		analyzed++
		currIO[pid] = writeBytes
		if prev, ok := prevIO[pid]; ok && elapsed > 0 {
			speed := float64(writeBytes-prev) / elapsed
			if speed > 0 {
				writers = append(writers, pidWrite{
					pid:   pid,
					speed: speed,
					name:  parseProcName(pid),
				})
			}
		}
	}

	// Use sort.Slice instead of insertion sort — O(n log n) vs O(n²)
	sort.Slice(writers, func(i, j int) bool {
		return writers[i].speed > writers[j].speed
	})

	top := 5
	if len(writers) < top {
		top = len(writers)
	}
	result := make([]DiskWriter, top)
	for i := 0; i < top; i++ {
		result[i] = DiskWriter{
			PID:        writers[i].pid,
			Name:       writers[i].name,
			WriteSpeed: writers[i].speed,
		}
	}
	return result, currIO
}

// ── Main poller goroutine ─────────────────────────────────────────────────────

func startSystemMonitor(bus *Bus, interval time.Duration) {
	go func() {
		log.Printf("[sysmon] starting system monitor (interval=%v)", interval)

		// Initial samples
		prevCPU, _ := parseCPUStat()
		prevDisk, _ := parseDiskStats()
		prevNet, _ := parseNetDev()
		prevIO := make(map[int]uint64)
		prevTime := time.Now()

		// warm-up: collect initial IO counts
		_, prevIO = getTopDiskWriters(prevIO, 0)

		time.Sleep(interval)

		// Circuit breaker
		consecutiveErrors := 0
		maxErrors := 3

		for {
			if consecutiveErrors >= maxErrors {
				log.Printf("[sysmon] circuit breaker open: too many errors, backing off 30s")
				time.Sleep(30 * time.Second)
				consecutiveErrors = 0
				continue
			}

			now := time.Now()
			elapsed := now.Sub(prevTime).Seconds()
			prevTime = now

			err := runMonitorCycle(bus, elapsed, prevCPU, prevDisk, prevNet, prevIO, &prevCPU, &prevDisk, &prevNet, &prevIO)
			if err != nil {
				consecutiveErrors++
				log.Printf("[sysmon] error: %v", err)
			} else {
				consecutiveErrors = 0
			}

			time.Sleep(interval)
		}
	}()
}

func runMonitorCycle(bus *Bus, elapsed float64,
	prevCPU []cpuSample, prevDisk map[string]diskSample, prevNet map[string]netSample, prevIO map[int]uint64,
	outCPU *[]cpuSample, outDisk *map[string]diskSample, outNet *map[string]netSample, outIO *map[int]uint64,
) error {
	now := time.Now()

	// CPU
	currCPU, err := parseCPUStat()
	var cpuPct float64
	var perCore []float64
	if err != nil {
		return fmt.Errorf("cpu parse: %w", err)
	}
	cpuPct, perCore = calcCPUPercent(prevCPU, currCPU)
	*outCPU = currCPU

	// Memory
	ramUsed, ramTotal, swapUsed, swapTotal, err := parseMemInfo()
	if err != nil {
		return fmt.Errorf("meminfo parse: %w", err)
	}
	ramPct := 0.0
	swapPct := 0.0
	if ramTotal > 0 {
		ramPct = 100.0 * float64(ramUsed) / float64(ramTotal)
	}
	if swapTotal > 0 {
		swapPct = 100.0 * float64(swapUsed) / float64(swapTotal)
	}

	// Disk
	currDisk, _ := parseDiskStats()
	diskRead, diskWrite := calcDiskSpeed(prevDisk, currDisk, elapsed)
	*outDisk = currDisk

	// Network
	currNet, _ := parseNetDev()
	netDL, netUL := calcNetSpeed(prevNet, currNet, elapsed)
	*outNet = currNet

	// Process count
	procCount := countProcesses()

	// Disk writers
	topWriters, currIO := getTopDiskWriters(prevIO, elapsed)
	*outIO = currIO

	// Publish metrics
	metrics := SystemMetrics{
		Type:             "system_metrics",
		Timestamp:        float64(now.UnixMilli()) / 1000.0,
		CPUPercent:       cpuPct,
		PerCoreCPU:       perCore,
		RAMUsedPercent:   ramPct,
		RAMUsed:          ramUsed,
		RAMTotal:         ramTotal,
		SwapUsedPercent:  swapPct,
		DiskReadSpeed:    diskRead,
		DiskWriteSpeed:   diskWrite,
		NetDownloadSpeed: netDL,
		NetUploadSpeed:   netUL,
		ProcessCount:     procCount,
	}

	if data, err := json.Marshal(metrics); err == nil {
		t := bus.getOrCreate("gtt.system.metrics")
		msg := Message{
			Topic:  "gtt.system.metrics",
			Mime:   "application/json",
			Data:   string(data),
			Sender: "kernclip-busd/sysmon",
		}
		published := t.publish(msg)
		bus.patterns.FanOut("gtt.system.metrics", published)
	}

	// Publish top disk writers
	if len(topWriters) > 0 {
		if data, err := json.Marshal(topWriters); err == nil {
			t := bus.getOrCreate("gtt.system.top_disk_writers")
			msg := Message{
				Topic:  "gtt.system.top_disk_writers",
				Mime:   "application/json",
				Data:   string(data),
				Sender: "kernclip-busd/sysmon",
			}
			published := t.publish(msg)
			bus.patterns.FanOut("gtt.system.top_disk_writers", published)
		}
	}

	return nil
}

// sysmonConfigPath returns the default config path for sysmon overrides.
func sysmonConfigPath() string {
	return filepath.Join(os.Getenv("HOME"), ".config", "kernclip", "sysmon.toml")
}
