package claudesdk

import (
	"context"
	"encoding/json"
	"fmt"
)

// MCPServer is an MCP server that runs in this process: Claude sees its tools as
// mcp__<Name>__<tool>, and calls them over the control protocol (mcp_message requests).
type MCPServer struct {
	Name    string
	Version string // "1.0.0" when empty
	Tools   []Tool
}

// Tool is one tool of an in-process MCP server.
type Tool struct {
	Name        string
	Description string
	// InputSchema is a JSON Schema object for the arguments ({"type":"object", ...}).
	InputSchema json.RawMessage
	// ReadOnly marks a tool that changes nothing (the readOnlyHint annotation).
	ReadOnly bool
	// AlwaysLoad keeps the tool in Claude's context, so it isn't deferred behind a tool search.
	AlwaysLoad bool
	// Handler runs the tool. ctx is cancelled when the CLI withdraws the call. An error is reported to
	// Claude as a failed call.
	Handler func(ctx context.Context, args json.RawMessage) (ToolResult, error)
}

// ToolResult is what a tool call returns to Claude.
type ToolResult struct {
	Text    string
	IsError bool
}

// mcpConfig is the --mcp-config value: the caller's servers, then the in-process ones, which the CLI
// reaches through this SDK.
func (o *Options) mcpConfig() (string, error) {
	if len(o.MCPServers) == 0 {
		return o.MCPConfig, nil
	}
	servers := map[string]any{}
	if o.MCPConfig != "" {
		var cfg struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(o.MCPConfig), &cfg); err != nil {
			return "", fmt.Errorf("claudesdk: MCPConfig: %w", err)
		}
		for k, v := range cfg.MCPServers {
			servers[k] = v
		}
	}
	for _, s := range o.MCPServers {
		servers[s.Name] = map[string]any{"type": "sdk", "name": s.Name}
	}
	b, err := json.Marshal(map[string]any{"mcpServers": servers})
	return string(b), err
}

// rpcMessage is a JSON-RPC message from the CLI's MCP client.
type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// mcpMessage answers an mcp_message control request: a JSON-RPC message for one of our servers.
func (c *Client) mcpMessage(ctx context.Context, raw json.RawMessage) (any, error) {
	var req struct {
		ServerName string     `json:"server_name"`
		Message    rpcMessage `json:"message"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	m := req.Message
	reply := func(result any) (any, error) {
		return map[string]any{"mcp_response": map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result}}, nil
	}
	fail := func(code int, msg string) (any, error) {
		return map[string]any{"mcp_response": map[string]any{"jsonrpc": "2.0", "id": m.ID,
			"error": map[string]any{"code": code, "message": msg}}}, nil
	}
	var srv *MCPServer
	for _, s := range c.opts.MCPServers {
		if s.Name == req.ServerName {
			srv = s
		}
	}
	if srv == nil {
		return fail(-32601, fmt.Sprintf("Server '%s' not found", req.ServerName))
	}
	if len(m.ID) == 0 {
		return reply(map[string]any{}) // a notification: nothing to answer, but the request wants an ack
	}
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		version := srv.Version
		if version == "" {
			version = "1.0.0"
		}
		return reply(map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": srv.Name, "version": version},
		})
	case "ping":
		return reply(map[string]any{})
	case "tools/list":
		tools := make([]map[string]any, 0, len(srv.Tools))
		for _, t := range srv.Tools {
			schema := t.InputSchema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			entry := map[string]any{"name": t.Name, "description": t.Description, "inputSchema": schema}
			if t.ReadOnly {
				entry["annotations"] = map[string]any{"readOnlyHint": true}
			}
			if t.AlwaysLoad {
				entry["_meta"] = map[string]any{"anthropic/alwaysLoad": true}
			}
			tools = append(tools, entry)
		}
		return reply(map[string]any{"tools": tools})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return fail(-32602, "invalid params")
		}
		for _, t := range srv.Tools {
			if t.Name != p.Name {
				continue
			}
			args := p.Arguments
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			res, err := t.Handler(ctx, args)
			if err != nil {
				res = ToolResult{Text: err.Error(), IsError: true}
			}
			out := map[string]any{"content": []map[string]any{{"type": "text", "text": res.Text}}}
			if res.IsError {
				out["isError"] = true
			}
			return reply(out)
		}
		return fail(-32602, fmt.Sprintf("Tool '%s' not found", p.Name))
	}
	return fail(-32601, fmt.Sprintf("Method '%s' not supported", m.Method))
}
