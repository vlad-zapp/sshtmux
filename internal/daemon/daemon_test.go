package daemon

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vlad-zapp/sshtmux/internal/session"
	"github.com/vlad-zapp/sshtmux/internal/tmux"
)

func testFactory(ctx context.Context, host, user string) (*session.Session, error) {
	ctrl := &noopController{paneID: "%0", alive: true, outputCh: make(chan string, 1024)}
	return session.NewFromController(ctrl, host, user), nil
}

// hangingController is like noopController but never completes commands.
// It does NOT set rvValue on Enter, so the executor polls forever until timeout.
type hangingController struct {
	noopController
}

func (c *hangingController) SendCommand(ctx context.Context, cmd string) (*tmux.CommandResult, error) {
	// Handle send-keys Enter without setting rvValue (command "hangs")
	if strings.HasPrefix(cmd, "send-keys") && strings.HasSuffix(cmd, "Enter") {
		return &tmux.CommandResult{}, nil
	}
	return c.noopController.SendCommand(ctx, cmd)
}

func startTestDaemon(t *testing.T) (*Daemon, string) {
	t.Helper()
	pool := NewConnPool(testFactory, 5*time.Minute)
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	d, err := NewDaemon(pool, sockPath, 30*time.Second)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	go d.Serve()
	// Wait for socket to be ready
	time.Sleep(10 * time.Millisecond)
	return d, sockPath
}

func sendRequest(t *testing.T, sockPath string, req Request) Response {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := WriteMessage(conn, &req); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	// Read messages, collecting streamed logs until the final response.
	var allLogs strings.Builder
	for {
		var resp Response
		if err := ReadMessage(conn, &resp); err != nil {
			t.Fatalf("ReadMessage: %v", err)
		}
		if resp.Streaming {
			allLogs.WriteString(resp.Logs)
			continue
		}
		// Final response — attach collected streamed logs
		if allLogs.Len() > 0 && resp.Logs == "" {
			resp.Logs = allLogs.String()
		}
		return resp
	}
}

func TestDaemonExec(t *testing.T) {
	d, sockPath := startTestDaemon(t)
	defer d.Stop()

	resp := sendRequest(t, sockPath, Request{
		Type:    "exec",
		Host:    "host1",
		User:    "user1",
		Command: "echo hello",
	})

	// With noop controller, output will be empty but should succeed
	if resp.Error != "" {
		t.Errorf("Error = %q, want empty", resp.Error)
	}
	if resp.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", resp.ExitCode)
	}
}

func TestDaemonStatus(t *testing.T) {
	d, sockPath := startTestDaemon(t)
	defer d.Stop()

	// Create a session first
	sendRequest(t, sockPath, Request{
		Type:    "exec",
		Host:    "host1",
		User:    "user1",
		Command: "ls",
	})

	resp := sendRequest(t, sockPath, Request{Type: "status"})
	if !resp.Success {
		t.Errorf("Success = false, error = %q", resp.Error)
	}
	if resp.Output == "" || resp.Output == "null" {
		t.Error("expected non-empty status output")
	}
}

func TestDaemonDisconnect(t *testing.T) {
	d, sockPath := startTestDaemon(t)
	defer d.Stop()

	// Create a session
	sendRequest(t, sockPath, Request{
		Type:    "exec",
		Host:    "host1",
		User:    "user1",
		Command: "ls",
	})

	// Disconnect
	resp := sendRequest(t, sockPath, Request{
		Type: "disconnect",
		Host: "host1",
		User: "user1",
	})
	if !resp.Success {
		t.Errorf("Disconnect failed: %s", resp.Error)
	}

	// Status should be empty
	resp = sendRequest(t, sockPath, Request{Type: "status"})
	if resp.Output != "[]" && resp.Output != "null" {
		t.Errorf("expected empty status, got %q", resp.Output)
	}
}

func TestDaemonUnknownType(t *testing.T) {
	d, sockPath := startTestDaemon(t)
	defer d.Stop()

	resp := sendRequest(t, sockPath, Request{Type: "bogus"})
	if resp.Error == "" {
		t.Error("expected error for unknown type")
	}
}

func TestDaemonShutdown(t *testing.T) {
	d, sockPath := startTestDaemon(t)

	resp := sendRequest(t, sockPath, Request{Type: "shutdown"})
	if !resp.Success {
		t.Errorf("Shutdown failed: %s", resp.Error)
	}

	// Give it time to shut down
	time.Sleep(200 * time.Millisecond)

	// Connection should be refused after shutdown
	_, err := net.Dial("unix", sockPath)
	if err == nil {
		t.Error("expected connection refused after shutdown")
	}
	_ = d
}

func TestDaemonConcurrentClients(t *testing.T) {
	d, sockPath := startTestDaemon(t)
	defer d.Stop()

	const numClients = 10
	errs := make(chan error, numClients)

	for i := range numClients {
		go func(n int) {
			conn, err := net.Dial("unix", sockPath)
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()

			req := Request{
				Type:    "exec",
				Host:    "host1",
				User:    "user1",
				Command: "echo test",
			}
			if err := WriteMessage(conn, &req); err != nil {
				errs <- err
				return
			}
			var resp Response
			if err := ReadMessage(conn, &resp); err != nil {
				errs <- err
				return
			}
			if resp.Error != "" {
				errs <- fmt.Errorf("response error: %s", resp.Error)
				return
			}
			errs <- nil
		}(i)
	}

	for range numClients {
		if err := <-errs; err != nil {
			t.Errorf("client error: %v", err)
		}
	}
}

func TestDaemonConcurrentVerboseRequests(t *testing.T) {
	d, sockPath := startTestDaemon(t)
	defer d.Stop()

	// Send concurrent requests with different Verbose flags.
	// With per-request context logging, there are no race conditions.
	const numClients = 20
	errs := make(chan error, numClients)

	for i := range numClients {
		go func(n int) {
			verbose := n%2 == 0
			conn, err := net.Dial("unix", sockPath)
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()

			req := Request{
				Type:    "exec",
				Host:    "host1",
				User:    "user1",
				Command: "echo test",
				Verbose: verbose,
			}
			if err := WriteMessage(conn, &req); err != nil {
				errs <- err
				return
			}
			// Read all messages (streaming logs + final response)
			for {
				var resp Response
				if err := ReadMessage(conn, &resp); err != nil {
					errs <- err
					return
				}
				if resp.Streaming {
					continue // consume log messages
				}
				if resp.Error != "" {
					errs <- fmt.Errorf("response error: %s", resp.Error)
					return
				}
				errs <- nil
				return
			}
		}(i)
	}

	for range numClients {
		if err := <-errs; err != nil {
			t.Errorf("client error: %v", err)
		}
	}
}

func TestDaemonVerboseReturnsLogs(t *testing.T) {
	d, sockPath := startTestDaemon(t)
	defer d.Stop()

	// Non-verbose request should have no logs
	resp := sendRequest(t, sockPath, Request{
		Type:    "exec",
		Host:    "host1",
		User:    "user1",
		Command: "ls",
		Verbose: false,
	})
	if resp.Logs != "" {
		t.Errorf("non-verbose request should have no logs, got %q", resp.Logs)
	}

	// Verbose request should return logs
	resp = sendRequest(t, sockPath, Request{
		Type:    "exec",
		Host:    "host1",
		User:    "user1",
		Command: "ls",
		Verbose: true,
	})
	if resp.Logs == "" {
		t.Error("verbose request should return daemon logs")
	}
	if !strings.Contains(resp.Logs, "daemon: dispatch") {
		t.Errorf("logs should contain dispatch message, got %q", resp.Logs)
	}

	// Another non-verbose request should again have no logs
	resp = sendRequest(t, sockPath, Request{
		Type:    "exec",
		Host:    "host1",
		User:    "user1",
		Command: "ls",
		Verbose: false,
	})
	if resp.Logs != "" {
		t.Errorf("non-verbose request after verbose should have no logs, got %q", resp.Logs)
	}
}

func TestDaemonExecTimeoutEvictsSession(t *testing.T) {
	var createCount atomic.Int32
	factory := func(ctx context.Context, host, user string) (*session.Session, error) {
		createCount.Add(1)
		ctrl := &hangingController{
			noopController: noopController{paneID: "%0", alive: true, outputCh: make(chan string, 1024)},
		}
		return session.NewFromController(ctrl, host, user), nil
	}

	pool := NewConnPool(factory, 5*time.Minute)
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	d, err := NewDaemon(pool, sockPath, 500*time.Millisecond) // short timeout
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	defer d.Stop()
	go d.Serve()
	time.Sleep(10 * time.Millisecond)

	// First exec should timeout because hangingController never completes
	resp := sendRequest(t, sockPath, Request{
		Type:    "exec",
		Host:    "host1",
		User:    "user1",
		Command: "sleep 100",
	})
	if resp.Error == "" {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(resp.Error, "deadline exceeded") {
		t.Errorf("expected deadline exceeded error, got %q", resp.Error)
	}

	// Session should have been evicted from the pool
	status := pool.Status()
	if len(status) != 0 {
		t.Errorf("expected 0 sessions after timeout eviction, got %d", len(status))
	}

	// Verify factory was called once for the first attempt
	if createCount.Load() != 1 {
		t.Errorf("createCount = %d, want 1", createCount.Load())
	}
}

func TestDaemonExecTimeoutCreatesNewSessionOnRetry(t *testing.T) {
	var createCount atomic.Int32
	factory := func(ctx context.Context, host, user string) (*session.Session, error) {
		createCount.Add(1)
		// First call returns a hanging controller, subsequent calls return normal
		if createCount.Load() == 1 {
			ctrl := &hangingController{
				noopController: noopController{paneID: "%0", alive: true, outputCh: make(chan string, 1024)},
			}
			return session.NewFromController(ctrl, host, user), nil
		}
		ctrl := &noopController{paneID: "%0", alive: true, outputCh: make(chan string, 1024)}
		return session.NewFromController(ctrl, host, user), nil
	}

	pool := NewConnPool(factory, 5*time.Minute)
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	d, err := NewDaemon(pool, sockPath, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	defer d.Stop()
	go d.Serve()
	time.Sleep(10 * time.Millisecond)

	// First exec times out
	resp := sendRequest(t, sockPath, Request{
		Type:    "exec",
		Host:    "host1",
		User:    "user1",
		Command: "sleep 100",
	})
	if resp.Error == "" {
		t.Fatal("expected timeout error on first exec")
	}

	// Second exec should create a new session and succeed
	resp = sendRequest(t, sockPath, Request{
		Type:    "exec",
		Host:    "host1",
		User:    "user1",
		Command: "echo hello",
	})
	if resp.Error != "" {
		t.Errorf("expected success on retry, got error: %s", resp.Error)
	}
	if createCount.Load() != 2 {
		t.Errorf("createCount = %d, want 2 (new session after eviction)", createCount.Load())
	}
}
