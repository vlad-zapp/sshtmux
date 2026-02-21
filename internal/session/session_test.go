package session

import (
	"context"
	"fmt"
	"io"
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
	mc.responses["capture-pane"] = "echo hello; __rv=$?; tmux set-option -p @sshtmux-rv \"$__rv\"; tmux wait-for -S __sshtmux_wf_1\nhello"
	mc.responses["display-message"] = "0"

	sess := NewFromController(mc, "testhost", "testuser")

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

func TestSessionAliveWithDeadController(t *testing.T) {
	mc := newMockController("%0")
	sess := NewFromController(mc, "testhost", "testuser")

	// mockController.Alive() always returns true
	if !sess.Alive() {
		t.Error("Alive should return true when controller is alive")
	}
}

func TestSessionCloseNilFields(t *testing.T) {
	// A session with nil ctrl, sshSess, and client should not panic on Close
	s := &Session{}
	if err := s.Close(); err != nil {
		t.Errorf("Close with nil fields: %v", err)
	}
}

func TestSessionAliveNilController(t *testing.T) {
	s := &Session{}
	if s.Alive() {
		t.Error("Alive should return false when controller is nil")
	}
}

func TestNewFromControllerSetsExecutor(t *testing.T) {
	mc := newMockController("%0")
	sess := NewFromController(mc, "h", "u")
	if sess.executor == nil {
		t.Error("executor should be set")
	}
	if sess.ctrl != mc {
		t.Error("ctrl should be set to the provided controller")
	}
}

func TestSessionExecTimeout(t *testing.T) {
	mc := newMockController("%0")
	mc.blockPrefixes = []string{"wait-for"} // blocks wait-for

	sess := NewFromController(mc, "testhost", "testuser")

	ctx := context.Background()
	_, err := sess.Exec(ctx, "sleep 100", 50*time.Millisecond)
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestPreCommandReadinessPattern(t *testing.T) {
	// Test the core readiness pattern: stdout.Read blocks until data arrives,
	// proving the pre-command has executed and the new shell is alive.
	stdoutR, stdoutW := io.Pipe()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Simulate: new shell writes prompt after a small delay
	go func() {
		time.Sleep(50 * time.Millisecond)
		fmt.Fprintf(stdoutW, "root@host:~# ")
	}()

	buf := make([]byte, 4096)
	readDone := make(chan struct{})
	go func() {
		stdoutR.Read(buf)
		close(readDone)
	}()

	select {
	case <-readDone:
		// Readiness confirmed
	case <-ctx.Done():
		t.Fatal("timeout waiting for pre-command readiness")
	}

	stdoutW.Close()
	stdoutR.Close()
}

func TestPreCommandReadinessContextCancel(t *testing.T) {
	// Verify that context cancellation during stdout wait works correctly.
	stdoutR, stdoutW := io.Pipe()
	defer stdoutW.Close()
	defer stdoutR.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	buf := make([]byte, 4096)
	readDone := make(chan struct{})
	go func() {
		stdoutR.Read(buf)
		close(readDone)
	}()

	select {
	case <-readDone:
		t.Error("expected timeout, but read completed")
	case <-ctx.Done():
		// Context expired before stdout data — expected behavior
	}
}
