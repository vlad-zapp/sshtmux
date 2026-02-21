package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/vlad-zapp/sshtmux/internal/daemon"
)

type mockSender struct {
	lastRequest daemon.Request
	response    *daemon.Response
	err         error
}

func (m *mockSender) Send(req daemon.Request) (*daemon.Response, error) {
	m.lastRequest = req
	if m.err != nil {
		return nil, m.err
	}
	return m.response, nil
}

func TestNewServer(t *testing.T) {
	ms := &mockSender{response: &daemon.Response{Success: true}}
	srv := New(ms)
	if srv == nil {
		t.Fatal("New returned nil")
	}
	if srv.server == nil {
		t.Fatal("server is nil")
	}
}

func TestExecToolDaemonError(t *testing.T) {
	ms := &mockSender{
		response: &daemon.Response{
			Error: "connection refused",
		},
	}
	srv := New(ms)

	if srv.sender != ms {
		t.Error("sender not set correctly")
	}
}

func TestMockSenderExec(t *testing.T) {
	ms := &mockSender{
		response: &daemon.Response{
			Success:  true,
			Output:   "test output",
			ExitCode: 0,
		},
	}

	resp, err := ms.Send(daemon.Request{
		Type:    "exec",
		Host:    "host1",
		User:    "user1",
		Command: "ls",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !resp.Success {
		t.Error("expected success")
	}
	if resp.Output != "test output" {
		t.Errorf("Output = %q, want %q", resp.Output, "test output")
	}
	if ms.lastRequest.Type != "exec" {
		t.Errorf("Type = %q, want %q", ms.lastRequest.Type, "exec")
	}
	if ms.lastRequest.Host != "host1" {
		t.Errorf("Host = %q, want %q", ms.lastRequest.Host, "host1")
	}
}

func TestHandleExecSuccess(t *testing.T) {
	ms := &mockSender{
		response: &daemon.Response{
			Success:  true,
			Output:   "file1.txt\nfile2.txt",
			ExitCode: 0,
		},
	}
	srv := New(ms)

	result, _, err := srv.handleExec(context.Background(), &mcp.CallToolRequest{}, ExecInput{
		Host:    "myhost",
		Command: "ls",
		User:    "myuser",
	})
	if err != nil {
		t.Fatalf("handleExec: %v", err)
	}
	if result.IsError {
		t.Error("expected no error")
	}
	if len(result.Content) != 1 {
		t.Fatalf("content length = %d, want 1", len(result.Content))
	}
	tc, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] type = %T, want *mcp.TextContent", result.Content[0])
	}
	if tc.Text != "file1.txt\nfile2.txt" {
		t.Errorf("Text = %q, want %q", tc.Text, "file1.txt\nfile2.txt")
	}

	// Verify the request was formed correctly
	if ms.lastRequest.Type != "exec" {
		t.Errorf("Type = %q, want %q", ms.lastRequest.Type, "exec")
	}
	if ms.lastRequest.Host != "myhost" {
		t.Errorf("Host = %q, want %q", ms.lastRequest.Host, "myhost")
	}
	if ms.lastRequest.User != "myuser" {
		t.Errorf("User = %q, want %q", ms.lastRequest.User, "myuser")
	}
	if ms.lastRequest.Command != "ls" {
		t.Errorf("Command = %q, want %q", ms.lastRequest.Command, "ls")
	}
}

func TestHandleExecNonZeroExit(t *testing.T) {
	ms := &mockSender{
		response: &daemon.Response{
			Success:  false,
			Output:   "No such file",
			ExitCode: 1,
		},
	}
	srv := New(ms)

	result, _, err := srv.handleExec(context.Background(), &mcp.CallToolRequest{}, ExecInput{
		Host:    "myhost",
		Command: "cat missing.txt",
	})
	if err != nil {
		t.Fatalf("handleExec: %v", err)
	}
	if result.IsError {
		t.Error("non-zero exit code should not set IsError (it's a valid result)")
	}
	tc := result.Content[0].(*mcp.TextContent)
	if !strings.Contains(tc.Text, "[exit code: 1]") {
		t.Errorf("Text should contain exit code prefix, got %q", tc.Text)
	}
	if !strings.Contains(tc.Text, "No such file") {
		t.Errorf("Text should contain output, got %q", tc.Text)
	}
}

func TestHandleExecDaemonSendError(t *testing.T) {
	ms := &mockSender{
		err: fmt.Errorf("connection refused"),
	}
	srv := New(ms)

	result, _, err := srv.handleExec(context.Background(), &mcp.CallToolRequest{}, ExecInput{
		Host:    "myhost",
		Command: "ls",
	})
	if err != nil {
		t.Fatalf("handleExec should not return Go error: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError=true for daemon send failure")
	}
	tc := result.Content[0].(*mcp.TextContent)
	if !strings.Contains(tc.Text, "connection refused") {
		t.Errorf("Text should contain error, got %q", tc.Text)
	}
}

func TestHandleExecResponseError(t *testing.T) {
	ms := &mockSender{
		response: &daemon.Response{
			Error: "session timeout",
		},
	}
	srv := New(ms)

	result, _, err := srv.handleExec(context.Background(), &mcp.CallToolRequest{}, ExecInput{
		Host:    "myhost",
		Command: "ls",
	})
	if err != nil {
		t.Fatalf("handleExec: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError=true for response error")
	}
	tc := result.Content[0].(*mcp.TextContent)
	if tc.Text != "session timeout" {
		t.Errorf("Text = %q, want %q", tc.Text, "session timeout")
	}
}

func TestHandleExecZeroExitEmptyOutput(t *testing.T) {
	ms := &mockSender{
		response: &daemon.Response{
			Success:  true,
			Output:   "",
			ExitCode: 0,
		},
	}
	srv := New(ms)

	result, _, err := srv.handleExec(context.Background(), &mcp.CallToolRequest{}, ExecInput{
		Host:    "myhost",
		Command: "true",
	})
	if err != nil {
		t.Fatalf("handleExec: %v", err)
	}
	if result.IsError {
		t.Error("expected no error")
	}
	tc := result.Content[0].(*mcp.TextContent)
	if tc.Text != "" {
		t.Errorf("Text = %q, want empty", tc.Text)
	}
}

func TestHandleExecStripsANSI(t *testing.T) {
	ms := &mockSender{
		response: &daemon.Response{
			Success:  true,
			Output:   "\x1b[1m\x1b[31mcolored output\x1b[0m",
			ExitCode: 0,
		},
	}
	srv := New(ms)

	result, _, err := srv.handleExec(context.Background(), &mcp.CallToolRequest{}, ExecInput{
		Host:    "myhost",
		Command: "ls --color",
	})
	if err != nil {
		t.Fatalf("handleExec: %v", err)
	}
	tc := result.Content[0].(*mcp.TextContent)
	if tc.Text != "colored output" {
		t.Errorf("Text = %q, want %q (ANSI should be stripped)", tc.Text, "colored output")
	}
}

func TestHandleExecStripsANSIWithExitCode(t *testing.T) {
	ms := &mockSender{
		response: &daemon.Response{
			Success:  false,
			Output:   "\x1b[31merror message\x1b[0m",
			ExitCode: 1,
		},
	}
	srv := New(ms)

	result, _, err := srv.handleExec(context.Background(), &mcp.CallToolRequest{}, ExecInput{
		Host:    "myhost",
		Command: "failing-cmd",
	})
	if err != nil {
		t.Fatalf("handleExec: %v", err)
	}
	tc := result.Content[0].(*mcp.TextContent)
	if strings.Contains(tc.Text, "\x1b[") {
		t.Errorf("Text still contains ANSI sequences: %q", tc.Text)
	}
	if !strings.Contains(tc.Text, "[exit code: 1]") {
		t.Errorf("Text should contain exit code prefix, got %q", tc.Text)
	}
	if !strings.Contains(tc.Text, "error message") {
		t.Errorf("Text should contain error message, got %q", tc.Text)
	}
}

func TestErrorResult(t *testing.T) {
	result := errorResult("something broke")
	if !result.IsError {
		t.Error("expected IsError=true")
	}
	if len(result.Content) != 1 {
		t.Fatalf("content length = %d, want 1", len(result.Content))
	}
	tc := result.Content[0].(*mcp.TextContent)
	if tc.Text != "something broke" {
		t.Errorf("Text = %q, want %q", tc.Text, "something broke")
	}
}
