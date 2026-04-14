package main

import (
	"fmt"
)

// ── Subscription modes ────────────────────────────────────────────────────────

// SubMode indicates how a subscription matches topics.
type SubMode int

const (
	// SubExact matches a single topic by exact name (original behavior).
	SubExact SubMode = iota
	// SubPattern matches topics by glob pattern (* and # wildcards).
	SubPattern
	// SubMulti matches an explicit list of exact topic names.
	SubMulti
)

// SubRequest represents a parsed subscription request.
type SubRequest struct {
	Mode    SubMode
	Topic   string   // SubExact
	Pattern string   // SubPattern
	Topics  []string // SubMulti
}

// ParseSubRequest determines the subscription mode from a JSON request.
//
// Three modes:
//   - Pattern: {"op":"sub","pattern":"agent.*"}
//   - Multi:   {"op":"sub","topics":["a","b","c"]}
//   - Exact:   {"op":"sub","topic":"my-topic"} — original behavior
func ParseSubRequest(req map[string]interface{}) (SubRequest, error) {
	// Pattern mode: {"op":"sub","pattern":"agent.*"}
	if p, ok := req["pattern"].(string); ok {
		// Validate pattern syntax
		_, err := CompilePattern(p)
		if err != nil {
			return SubRequest{}, fmt.Errorf("invalid pattern: %w", err)
		}
		return SubRequest{Mode: SubPattern, Pattern: p}, nil
	}

	// Multi mode: {"op":"sub","topics":["a","b","c"]}
	if t, ok := req["topics"].([]interface{}); ok {
		topics := make([]string, 0, len(t))
		for i, v := range t {
			s, ok := v.(string)
			if !ok {
				return SubRequest{}, fmt.Errorf("topics[%d] must be string", i)
			}
			topics = append(topics, s)
		}
		if len(topics) == 0 {
			return SubRequest{}, fmt.Errorf("topics must not be empty")
		}
		return SubRequest{Mode: SubMulti, Topics: topics}, nil
	}

	// Exact mode: {"op":"sub","topic":"my-topic"} — original behavior
	if topic, ok := req["topic"].(string); ok {
		return SubRequest{Mode: SubExact, Topic: topic}, nil
	}

	return SubRequest{}, fmt.Errorf("sub requires topic, pattern, or topics")
}
