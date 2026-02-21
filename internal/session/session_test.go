package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vlad-zapp/sshtmux/internal/tmux"
)

func TestNewFromController(t *testing.T) {
	mc := newMockController("%0")
	mc.responses["display-message"] = "0"

	sess := NewFromController(mc, "testhost", "testuser")
	if sess.Host != "testhost" {
		t.Errorf("Host = %q, want %q", sess.Host, "testhost")
	}
	if sess.User != "testuser" {
		t.Errorf("User = %q, want %q", sess.User, "testuser")
	}
}

func TestSessionExec(t *testing.T) {
	mc := newMockController("%0")
	mc.responses["display-message"] = "0"

	sess := NewFromController(mc, "testhost", "testuser")

	go func() {
		time.Sleep(5 * time.Millisecond)
		mc.outputCh <- tmux.Notification{PaneID: "%0", Data: "hello\n"}
		mc.outputCh <- tmux.Notification{PaneID: "%0", Data: "$ "}
	}()

	ctx := context.Background()
	result, err := sess.Exec(ctx, "echo hello", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
}

func TestSessionClose(t *testing.T) {
	mc := newMockController("%0")
	sess := NewFromController(mc, "testhost", "testuser")

	// Close should not panic
	if err := sess.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestSessionShellEscaping(t *testing.T) {
	// Verify that tmux.ShellQuote properly handles paths with spaces
	quoted := tmux.ShellQuote("/path/with spaces/tmux.sock")
	if !strings.Contains(quoted, "with spaces") {
		t.Errorf("ShellQuote should preserve path: %q", quoted)
	}
	if quoted[0] != '\'' || quoted[len(quoted)-1] != '\'' {
		t.Errorf("ShellQuote should wrap in single quotes: %q", quoted)
	}

	// Verify a path with single quotes is properly escaped
	quoted = tmux.ShellQuote("it's a path")
	if !strings.Contains(quoted, `'\''`) {
		t.Errorf("ShellQuote should escape single quotes: %q", quoted)
	}
}
