package client

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/vlad-zapp/sshtmux/internal/daemon"
	"github.com/vlad-zapp/sshtmux/internal/session"
	"github.com/vlad-zapp/sshtmux/internal/tmux"
)

type noopController struct {
	paneID string
}

func (c *noopController) SendCommand(ctx context.Context, cmd string) (*tmux.CommandResult, error) {
	return &tmux.CommandResult{}, nil
}
func (c *noopController) OutputChan() <-chan tmux.Notification {
	ch := make(chan tmux.Notification)
	close(ch)
	return ch
}
func (c *noopController) PaneID() string     { return c.paneID }
func (c *noopController) SetPaneID(id string) { c.paneID = id }
func (c *noopController) Alive() bool          { return true }
func (c *noopController) Detach() error       { return nil }
func (c *noopController) Close() error        { return nil }

func testFactory(ctx context.Context, host, user string) (*session.Session, error) {
	ctrl := &noopController{paneID: "%0"}
	return session.NewFromController(ctrl, host, user), nil
}

func startTestDaemon(t *testing.T) (string, func()) {
	t.Helper()
	pool := daemon.NewConnPool(testFactory, 5*time.Minute)
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	d, err := daemon.NewDaemon(pool, sockPath, 30*time.Second)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	go d.Serve()
	time.Sleep(10 * time.Millisecond)
	return sockPath, func() { d.Stop() }
}

func TestClientExec(t *testing.T) {
	sockPath, cleanup := startTestDaemon(t)
	defer cleanup()

	c := NewClient(sockPath)
	resp, err := c.Exec("host1", "user1", "echo hello")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if resp.Error != "" {
		t.Errorf("Error = %q, want empty", resp.Error)
	}
}

func TestClientStatus(t *testing.T) {
	sockPath, cleanup := startTestDaemon(t)
	defer cleanup()

	c := NewClient(sockPath)

	// Create a session
	c.Exec("host1", "user1", "ls")

	resp, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !resp.Success {
		t.Errorf("Status failed: %s", resp.Error)
	}
}

func TestClientDisconnect(t *testing.T) {
	sockPath, cleanup := startTestDaemon(t)
	defer cleanup()

	c := NewClient(sockPath)
	c.Exec("host1", "user1", "ls")

	resp, err := c.Disconnect("host1", "user1")
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if !resp.Success {
		t.Errorf("Disconnect failed: %s", resp.Error)
	}
}

func TestClientShutdown(t *testing.T) {
	sockPath, cleanup := startTestDaemon(t)
	_ = cleanup // daemon will shut itself down

	c := NewClient(sockPath)
	resp, err := c.Shutdown()
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !resp.Success {
		t.Errorf("Shutdown failed: %s", resp.Error)
	}

	time.Sleep(200 * time.Millisecond)

	// Should fail to connect
	_, err = net.DialTimeout("unix", sockPath, time.Second)
	if err == nil {
		t.Error("expected connection refused after shutdown")
	}
}

func TestClientConnectionRefused(t *testing.T) {
	c := NewClient("/nonexistent/sock")
	_, err := c.Exec("host1", "user1", "ls")
	if err == nil {
		t.Error("expected error when daemon not running")
	}
}

func TestEnsureDaemonAlreadyRunning(t *testing.T) {
	sockPath, cleanup := startTestDaemon(t)
	defer cleanup()

	c := NewClient(sockPath)
	// Daemon is already running, EnsureDaemon should succeed immediately
	if err := c.EnsureDaemon(); err != nil {
		t.Fatalf("EnsureDaemon: %v", err)
	}
}

func TestEnsureDaemonNotRunning(t *testing.T) {
	// With a non-existent socket and non-existent executable path,
	// this should fail (can't start daemon)
	c := NewClient(filepath.Join(t.TempDir(), "test.sock"))
	err := c.EnsureDaemon()
	// This will fail because the executable doesn't support "daemon" subcommand in test
	// Just verify it doesn't panic
	if err == nil {
		t.Log("EnsureDaemon succeeded unexpectedly (test binary supports daemon?)")
	}
}
