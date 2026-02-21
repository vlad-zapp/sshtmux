package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/vlad-zapp/sshtmux/internal/daemon"
)

// Sender sends requests to the daemon.
type Sender interface {
	Send(req daemon.Request) (*daemon.Response, error)
}

// Server wraps the MCP server.
type Server struct {
	sender Sender
	server *mcp.Server
}

// ExecInput is the input schema for the sshtmux_exec tool.
type ExecInput struct {
	Host    string `json:"host" jsonschema:"SSH host to connect to"`
	Command string `json:"command" jsonschema:"command to execute on the remote host"`
	User    string `json:"user,omitempty" jsonschema:"SSH user (optional)"`
}

// DisconnectInput is the input schema for the sshtmux_disconnect tool.
type DisconnectInput struct {
	Host string `json:"host" jsonschema:"SSH host to disconnect from"`
	User string `json:"user,omitempty" jsonschema:"SSH user (optional)"`
}

// StatusInput is the input schema for the sshtmux_status tool.
type StatusInput struct{}

// New creates a new MCP server that uses the given sender.
func New(sender Sender) *Server {
	s := &Server{sender: sender}
	s.server = mcp.NewServer(
		&mcp.Implementation{Name: "sshtmux", Version: "1.0.0"},
		nil,
	)

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "sshtmux_exec",
		Description: "Execute a command on a remote host via SSH + tmux. Connections are cached for reuse.",
	}, s.handleExec)

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "sshtmux_disconnect",
		Description: "Close a cached SSH connection to a remote host.",
	}, s.handleDisconnect)

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "sshtmux_status",
		Description: "List all active cached SSH connections.",
	}, s.handleStatus)

	return s
}

// Run starts the MCP server on stdio.
func (s *Server) Run() error {
	return s.server.Run(context.Background(), &mcp.StdioTransport{})
}

func (s *Server) handleExec(ctx context.Context, req *mcp.CallToolRequest, input ExecInput) (*mcp.CallToolResult, any, error) {
	resp, err := s.sender.Send(daemon.Request{
		Type:    "exec",
		Host:    input.Host,
		User:    input.User,
		Command: input.Command,
	})
	if err != nil {
		return errorResult(fmt.Sprintf("daemon error: %v", err)), nil, nil
	}
	if resp.Error != "" {
		return errorResult(resp.Error), nil, nil
	}

	text := resp.Output
	if resp.ExitCode != 0 {
		text = fmt.Sprintf("[exit code: %d]\n%s", resp.ExitCode, resp.Output)
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}, nil, nil
}

func (s *Server) handleDisconnect(ctx context.Context, req *mcp.CallToolRequest, input DisconnectInput) (*mcp.CallToolResult, any, error) {
	resp, err := s.sender.Send(daemon.Request{
		Type: "disconnect",
		Host: input.Host,
		User: input.User,
	})
	if err != nil {
		return errorResult(fmt.Sprintf("daemon error: %v", err)), nil, nil
	}
	if resp.Error != "" {
		return errorResult(resp.Error), nil, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "Disconnected"}},
	}, nil, nil
}

func (s *Server) handleStatus(ctx context.Context, req *mcp.CallToolRequest, input StatusInput) (*mcp.CallToolResult, any, error) {
	resp, err := s.sender.Send(daemon.Request{Type: "status"})
	if err != nil {
		return errorResult(fmt.Sprintf("daemon error: %v", err)), nil, nil
	}
	if resp.Error != "" {
		return errorResult(resp.Error), nil, nil
	}

	text := resp.Output
	if isEmptyJSONList(text) {
		text = "No active connections"
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}, nil, nil
}

func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
		IsError: true,
	}
}

// isEmptyJSONList checks if s is a JSON null or an empty JSON array.
func isEmptyJSONList(s string) bool {
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(s), &items); err != nil {
		return s == "null"
	}
	return len(items) == 0
}
