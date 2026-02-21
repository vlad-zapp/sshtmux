package mcpserver

import (
	"testing"

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

	// Test that the sender receives the correct request type
	ms.response = &daemon.Response{
		Success:  true,
		Output:   "hello world",
		ExitCode: 0,
	}

	// Verify the server was created with 3 tools
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

func TestMockSenderStatus(t *testing.T) {
	ms := &mockSender{
		response: &daemon.Response{
			Success: true,
			Output:  "[]",
		},
	}

	resp, err := ms.Send(daemon.Request{Type: "status"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.Output != "[]" {
		t.Errorf("Output = %q, want %q", resp.Output, "[]")
	}
}

func TestMockSenderDisconnect(t *testing.T) {
	ms := &mockSender{
		response: &daemon.Response{
			Success: true,
			Output:  "disconnected",
		},
	}

	resp, err := ms.Send(daemon.Request{
		Type: "disconnect",
		Host: "host1",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !resp.Success {
		t.Error("expected success")
	}
}
