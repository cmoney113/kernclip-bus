package main

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// ── Pattern subscription ──────────────────────────────────────────────────────

// PatternSub represents a wildcard subscription held by one connection.
type PatternSub struct {
	ID       string          // uuid
	Pattern  string          // original glob string e.g. "agent.*.file_written"
	Compiled *regexp.Regexp  // compiled regex for matching
	ConnID   string          // connection identifier
	Ch       chan Message    // buffered — never block publisher
}

// PatternRegistry holds all active pattern subscriptions.
type PatternRegistry struct {
	mu   sync.RWMutex
	subs map[string]*PatternSub // id → sub
}

// NewPatternRegistry creates an empty registry.
func NewPatternRegistry() *PatternRegistry {
	return &PatternRegistry{subs: make(map[string]*PatternSub)}
}

// CompilePattern converts a glob pattern to an anchored regex.
//
// Rules:
//   - * matches exactly one dot-separated segment (no dots)
//   - # matches one or more segments (must be terminal, i.e., last segment)
//
// Examples:
//   agent.*              → ^agent\.[^.]+$
//   agent.*.file_written → ^agent\.[^.]+\.file_written$
//   agent.#              → ^agent\..+$
func CompilePattern(pattern string) (*regexp.Regexp, error) {
	parts := strings.Split(pattern, ".")

	// Validate: # must be last segment if present
	for i, p := range parts {
		if p == "#" && i != len(parts)-1 {
			return nil, fmt.Errorf("# wildcard must be terminal segment")
		}
	}

	// Build regex segment by segment
	var sb strings.Builder
	sb.WriteString("^")
	for i, part := range parts {
		if i > 0 {
			sb.WriteString(`\.`)
		}
		switch part {
		case "*":
			sb.WriteString(`[^.]+`) // one segment, no dots
		case "#":
			sb.WriteString(`.+`) // one or more chars including dots
		default:
			sb.WriteString(regexp.QuoteMeta(part))
		}
	}
	sb.WriteString("$")

	return regexp.Compile(sb.String())
}

// Register adds a pattern subscription to the registry.
func (r *PatternRegistry) Register(sub *PatternSub) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subs[sub.ID] = sub
}

// Unregister removes a pattern subscription from the registry.
func (r *PatternRegistry) Unregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.subs, id)
}

// FanOut delivers msg to all pattern subs whose pattern matches the topic.
// Called on every pub — must be fast. Never blocks.
func (r *PatternRegistry) FanOut(topic string, msg Message) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, sub := range r.subs {
		if sub.Compiled.MatchString(topic) {
			select {
			case sub.Ch <- msg:
				// delivered
			default:
				// Slow subscriber — drop, never block publisher.
				// Publisher always wins. 2.3ns stays 2.3ns.
			}
		}
	}
}

// MatchingPatterns returns pattern subs that would match a given topic.
// Used by manifest to show subscription topology.
func (r *PatternRegistry) MatchingPatterns(topic string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var matched []string
	for _, sub := range r.subs {
		if sub.Compiled.MatchString(topic) {
			matched = append(matched, sub.Pattern)
		}
	}
	return matched
}

// AllPatterns returns all registered patterns (for manifest).
func (r *PatternRegistry) AllPatterns() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	patterns := make([]string, 0, len(r.subs))
	for _, sub := range r.subs {
		patterns = append(patterns, sub.Pattern)
	}
	return patterns
}

// Count returns the number of registered pattern subscriptions.
func (r *PatternRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.subs)
}
