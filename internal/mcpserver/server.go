package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/vlad-zapp/sshtmux/internal/daemon"
	"github.com/vlad-zapp/sshtmux/internal/session"
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

	text := session.StripANSI(resp.Output)
	if resp.ExitCode != 0 {
		text = fmt.Sprintf("[exit code: %d]\n%s", resp.ExitCode, text)
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
