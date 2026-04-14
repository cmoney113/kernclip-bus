package main

import (
	"os"
	"strconv"
	"strings"
)

// ── Config types ──────────────────────────────────────────────────────────────

// TopicOverride configures ring size for topics matching a prefix.
type TopicOverride struct {
	Prefix   string `json:"prefix"`
	RingSize int    `json:"ring_size"`
}

// Config holds daemon configuration loaded from TOML.
type Config struct {
	DefaultRingSize int             `json:"default_ring_size"`
	Overrides       []TopicOverride `json:"topic_overrides"`
}

// DefaultConfig provides sensible defaults when config file is missing or invalid.
var DefaultConfig = Config{
	DefaultRingSize: 128,
	Overrides: []TopicOverride{
		{Prefix: "agent.", RingSize: 4096},
		{Prefix: "sandbox.", RingSize: 1024},
		{Prefix: "pattern.", RingSize: 512},
		{Prefix: "hook.", RingSize: 512},
		{Prefix: "context.", RingSize: 256},
		{Prefix: "ai.", RingSize: 2048},
	},
}

// LoadConfig reads a TOML config file. Returns DefaultConfig on any error.
// Fail-open: never crash the daemon for a bad config file.
func LoadConfig(path string) Config {
	cfg := DefaultConfig

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}

	var current *TopicOverride

	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)

		// Skip blanks and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// New table entry: [[topic_overrides]]
		if line == "[[topic_overrides]]" {
			if current != nil {
				cfg.Overrides = append(cfg.Overrides, *current)
			}
			current = &TopicOverride{}
			continue
		}

		// Skip other table headers (e.g., [some_other_section])
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			continue
		}

		// Key = value
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])

		switch key {
		case "default_ring_size":
			cfg.DefaultRingSize = parseInt(val, cfg.DefaultRingSize)
		case "ring_size":
			if current != nil {
				current.RingSize = parseInt(val, 128)
			}
		case "prefix":
			if current != nil {
				// Strip surrounding quotes
				current.Prefix = strings.Trim(val, `"`)
			}
		}
	}

	// Don't forget the last entry
	if current != nil {
		cfg.Overrides = append(cfg.Overrides, *current)
	}

	return cfg
}

// RingSizeFor returns the configured ring size for a topic.
// Uses longest prefix match: the most specific override wins.
func (c *Config) RingSizeFor(topic string) int {
	best := ""
	size := c.DefaultRingSize
	for _, o := range c.Overrides {
		if strings.HasPrefix(topic, o.Prefix) && len(o.Prefix) > len(best) {
			best = o.Prefix
			size = o.RingSize
		}
	}
	return size
}

// parseInt parses an integer with fallback on error.
func parseInt(s string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fallback
	}
	return n
}
