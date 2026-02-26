package client

import (
	"context"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vlad-zapp/sshtmux/internal/daemon"
	"github.com/vlad-zapp/sshtmux/internal/session"
	"github.com/vlad-zapp/sshtmux/internal/tmux"
)

// Ensure noopController satisfies the tmux.Controller interface.
var _ tmux.Controller = (*noopController)(nil)

type noopController struct {
	paneID   string
	outputCh chan string
	rvValue  string
}

func (c *noopController) SendCommand(ctx context.Context, cmd string) (*tmux.CommandResult, error) {
	// Handle @sshtmux-rv unset
	if strings.Contains(cmd, "set-option -pu") && strings.Contains(cmd, "@sshtmux-rv") {
		c.rvValue = ""
		return &tmux.CommandResult{}, nil
	}
	// Handle @sshtmux-rv poll
	if strings.Contains(cmd, "display-message") && strings.Contains(cmd, "@sshtmux-rv") {
		return &tmux.CommandResult{Data: c.rvValue}, nil
	}
	// When receiving send-keys -H, simulate terminal echo by
	// decoding the hex bytes and sending them to outputCh.
	if strings.HasPrefix(cmd, "send-keys -H") {
		text := decodeHexSendKeys(cmd)
		if text != "" {
			go func() { c.outputCh <- text }()
		}
	} else if strings.HasPrefix(cmd, "send-keys") && strings.HasSuffix(cmd, "Enter") {
		// Simulate instant command completion via pane option.
		c.rvValue = "0"
	}
	return &tmux.CommandResult{}, nil
}
func (c *noopController) SendCommandPipeline(ctx context.Context, cmds []string) ([]*tmux.CommandResult, error) {
	results := make([]*tmux.CommandResult, len(cmds))
	for i, cmd := range cmds {
		r, err := c.SendCommand(ctx, cmd)
		if err != nil {
			return nil, err
		}
		results[i] = r
	}
	return results, nil
}
func (c *noopController) OutputCh() <-chan string { return c.outputCh }
func (c *noopController) PaneID() string           { return c.paneID }
func (c *noopController) SetPaneID(id string)      { c.paneID = id }
func (c *noopController) Alive() bool              { return true }
func (c *noopController) Detach() error            { return nil }
func (c *noopController) Close() error             { return nil }

// decodeHexSendKeys decodes a send-keys -H command back to text.
// Input format: "send-keys -H -t %0 65 63 68 6f ..."
// Ctrl+V bytes (0x16) are stripped as they are quoted-insert prefixes.
func decodeHexSendKeys(cmd string) string {
	parts := strings.SplitN(cmd, " ", 5) // send-keys -H -t %<id> <hex...>
	if len(parts) < 5 {
		return ""
	}
	tokens := strings.Fields(parts[4])
	var b strings.Builder
	for _, tok := range tokens {
		val, err := strconv.ParseUint(tok, 16, 8)
		if err != nil {
			continue
		}
		if val == 0x16 {
			continue
		}
		b.WriteByte(byte(val))
	}
	return b.String()
}

func testFactory(ctx context.Context, host, user string) (*session.Session, error) {
	ctrl := &noopController{paneID: "%0", outputCh: make(chan string, 1024)}
	return session.NewFromController(ctrl, host, user), nil
}

func startTestDaemon(t *testing.T) (string, func()) {
	t.Helper()
	pool := daemon.NewConnPool(testFactory, 5*time.Minute)
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	d, err := daemon.NewDaemon(pool, sockPath, 30*time.Second, 0)
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
	resp, err := c.Exec("host1", "user1", "echo hello", 0)
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
	c.Exec("host1", "user1", "ls", 0)

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
	c.Exec("host1", "user1", "ls", 0)

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
	_, err := c.Exec("host1", "user1", "ls", 0)
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
