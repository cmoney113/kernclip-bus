// kernclip-bus MCP server
//
// Architecture: pure protocol bridge — zero hardcoded tools.
//
// On every tools/list, this server calls {"op":"manifest"} on the bus daemon
// and dynamically generates MCP tool definitions from the response.
// On every tools/call, it routes to the bus socket using the op embedded
// in the tool definition.
//
// To add a new bus operation: add it to buildManifest() in main.go.
// This MCP server never needs to be changed.
//
// Wire: MCP JSON-RPC 2.0 over stdio (Claude Desktop standard).

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
)

// ── JSON-RPC types ────────────────────────────────────────────────────────────

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Result  any    `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// ── Bus manifest types (mirrors main.go) ──────────────────────────────────────

type OpParam struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
	Default     string `json:"default,omitempty"`
}

type OpDef struct {
	Op          string             `json:"op"`
	ToolName    string             `json:"tool_name"`
	Description string             `json:"description"`
	Streaming   bool               `json:"streaming"`
	Params      map[string]OpParam `json:"params"`
}

type Manifest struct {
	Version string  `json:"version"`
	Socket  string  `json:"socket"`
	Ops     []OpDef `json:"ops"`
}

// ── Bus client ────────────────────────────────────────────────────────────────

func sockPath() string {
	return fmt.Sprintf("/run/user/%d/kernclip-bus.sock", os.Getuid())
}

func busRoundtrip(req map[string]any) (map[string]any, error) {
	conn, err := net.Dial("unix", sockPath())
	if err != nil {
		return nil, fmt.Errorf("kernclip-busd not running (socket: %s)", sockPath())
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, err
	}
	var resp map[string]any
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func fetchManifest() (*Manifest, error) {
	resp, err := busRoundtrip(map[string]any{"op": "manifest"})
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(resp["manifest"])
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ── MCP tool generation from manifest ─────────────────────────────────────────
//
// This is the core of the "dynamic" architecture.
// OpDef → MCP InputSchema → Claude sees the tool with correct params.

type mcpTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema inputSchema `json:"inputSchema"`
	// internal: which bus op to call
	busOp string `json:"-"`
}

type inputSchema struct {
	Type       string              `json:"type"`
	Properties map[string]propDef  `json:"properties"`
	Required   []string            `json:"required,omitempty"`
}

type propDef struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	Default     string `json:"default,omitempty"`
}

func manifestToTools(m *Manifest) []mcpTool {
	tools := make([]mcpTool, 0, len(m.Ops))
	for _, op := range m.Ops {
		props := make(map[string]propDef, len(op.Params))
		var required []string
		for name, p := range op.Params {
			typ := p.Type
			if typ == "" {
				typ = "string"
			}
			props[name] = propDef{
				Type:        typ,
				Description: p.Description,
				Default:     p.Default,
			}
			if p.Required {
				required = append(required, name)
			}
		}
		tools = append(tools, mcpTool{
			Name:        op.ToolName,
			Description: op.Description,
			InputSchema: inputSchema{
				Type:       "object",
				Properties: props,
				Required:   required,
			},
			busOp: op.Op,
		})
	}
	return tools
}

// opForTool finds the bus op name for a given MCP tool name by checking manifest.
func opForTool(toolName string, m *Manifest) string {
	for _, op := range m.Ops {
		if op.ToolName == toolName {
			return op.Op
		}
	}
	return ""
}

// ── MCP handler ───────────────────────────────────────────────────────────────

func handle(req rpcRequest, enc *json.Encoder) {
	ok := func(result any) {
		enc.Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
	}
	fail := func(code int, msg string) {
		enc.Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: code, Message: msg}})
	}

	switch req.Method {

	case "initialize":
		// Echo back the client's protocol version so any client version is accepted
		var initParams struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		clientVersion := "2025-03-26"
		if req.Params != nil {
			if err := json.Unmarshal(req.Params, &initParams); err == nil && initParams.ProtocolVersion != "" {
				clientVersion = initParams.ProtocolVersion
			}
		}
		ok(map[string]any{
			"protocolVersion": clientVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "kernclip-bus", "version": "1.0"},
		})

	case "initialized", "notifications/initialized":
		// no-op notification, no response needed

	case "ping":
		ok(map[string]any{})

	case "tools/list":
		// Fetch manifest fresh every time — picks up new ops automatically
		m, err := fetchManifest()
		if err != nil {
			fail(-32603, fmt.Sprintf("bus unavailable: %v", err))
			return
		}
		tools := manifestToTools(m)
		// Encode without the internal busOp field
		type toolOut struct {
			Name        string      `json:"name"`
			Description string      `json:"description"`
			InputSchema inputSchema `json:"inputSchema"`
		}
		out := make([]toolOut, len(tools))
		for i, t := range tools {
			out[i] = toolOut{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}
		}
		ok(map[string]any{"tools": out})

	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			fail(-32600, "invalid params")
			return
		}

		// Resolve tool name → bus op via manifest
		m, err := fetchManifest()
		if err != nil {
			fail(-32603, fmt.Sprintf("bus unavailable: %v", err))
			return
		}
		busOp := opForTool(p.Name, m)
		if busOp == "" {
			fail(-32601, fmt.Sprintf("unknown tool: %s", p.Name))
			return
		}

		// Build bus request from tool arguments
		busReq := map[string]any{"op": busOp}
		for k, v := range p.Arguments {
			busReq[k] = v
		}

		resp, err := busRoundtrip(busReq)
		if err != nil {
			ok(map[string]any{
				"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("Error: %v", err)}},
				"isError": true,
			})
			return
		}

		// Format response as readable text for Claude
		text := formatBusResponse(resp)
		ok(map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
		})

	default:
		if strings.HasPrefix(req.Method, "notifications/") {
			return // silently ignore all notifications
		}
		fail(-32601, fmt.Sprintf("method not found: %s", req.Method))
	}
}

// formatBusResponse turns a bus response into readable text for Claude.
func formatBusResponse(r map[string]any) string {
	// Remove ok field, pretty-print the rest
	delete(r, "ok")
	if errMsg, ok := r["error"].(string); ok {
		return fmt.Sprintf("Error: %s", errMsg)
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	return string(b)
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	logF, _ := os.OpenFile("/tmp/kernclip-mcp.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	log := func(f string, a ...any) {
		if logF != nil {
			fmt.Fprintf(logF, f+"\n", a...)
		}
	}

	log("kernclip-bus MCP server starting (pid %d)", os.Getpid())

	reader := bufio.NewReader(os.Stdin)
	enc := json.NewEncoder(os.Stdout)

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				log("stdin closed, exiting")
				return
			}
			log("read error: %v", err)
			return
		}

		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			log("parse error: %v | input: %s", err, line)
			continue
		}

		log("→ %s (id=%v)", req.Method, req.ID)

		// notifications have no id and need no response
		if req.ID == nil && strings.HasPrefix(req.Method, "notifications/") {
			continue
		}

		handle(req, enc)
	}
}
