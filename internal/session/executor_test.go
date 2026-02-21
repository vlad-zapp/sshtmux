package session

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vlad-zapp/sshtmux/internal/tmux"
)

// mockController simulates a tmux controller for testing.
type mockController struct {
	mu       sync.Mutex
	paneID   string
	commands []string
	// responses maps command prefix to response data
	responses map[string]string
	// blockPrefixes: commands matching these prefixes block until context is done
	blockPrefixes []string
	outputCh      chan tmux.Notification
}

func newMockController(paneID string) *mockController {
	return &mockController{
		paneID:    paneID,
		responses: make(map[string]string),
		outputCh:  make(chan tmux.Notification, 256),
	}
}

func (m *mockController) SendCommand(ctx context.Context, cmd string) (*tmux.CommandResult, error) {
	m.mu.Lock()
	m.commands = append(m.commands, cmd)
	blocks := m.blockPrefixes
	m.mu.Unlock()

	// Check if this command should block
	for _, prefix := range blocks {
		if strings.HasPrefix(cmd, prefix) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	// Check for matching response
	m.mu.Lock()
	defer m.mu.Unlock()
	for prefix, data := range m.responses {
		if strings.HasPrefix(cmd, prefix) {
			return &tmux.CommandResult{Data: data}, nil
		}
	}
	return &tmux.CommandResult{}, nil
}

func (m *mockController) OutputChan() <-chan tmux.Notification {
	return m.outputCh
}

func (m *mockController) PaneID() string {
	return m.paneID
}

func (m *mockController) SetPaneID(id string) {
	m.paneID = id
}

func (m *mockController) Detach() error {
	return nil
}

func (m *mockController) Close() error {
	return nil
}

func (m *mockController) getCommands() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.commands))
	copy(out, m.commands)
	return out
}

func TestExecBasic(t *testing.T) {
	mc := newMockController("%0")
	mc.responses["display-message"] = "0" // exit code 0

	exec := NewExecutor(mc)

	// Simulate output arriving (use __sshtmux_wf_ channel format)
	go func() {
		time.Sleep(5 * time.Millisecond)
		mc.outputCh <- tmux.Notification{
			PaneID: "%0",
			Data:   "$ ls -la; __rv=$?; tmux set-option -p @sshtmux-rv \"$__rv\"; tmux wait-for -S __sshtmux_wf_1\n",
		}
		mc.outputCh <- tmux.Notification{
			PaneID: "%0",
			Data:   "file1.txt\nfile2.txt\n",
		}
		mc.outputCh <- tmux.Notification{
			PaneID: "%0",
			Data:   "$ ",
		}
	}()

	ctx := context.Background()
	result, err := exec.Exec(ctx, "ls -la", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if !strings.Contains(result.Output, "file1.txt") {
		t.Errorf("Output = %q, want to contain file1.txt", result.Output)
	}

	// Verify command sequence: send-keys, wait-for, display-message
	cmds := mc.getCommands()
	if len(cmds) != 3 {
		t.Fatalf("got %d commands, want 3: %v", len(cmds), cmds)
	}
	if !strings.HasPrefix(cmds[0], "send-keys") {
		t.Errorf("cmd[0] = %q, want send-keys prefix", cmds[0])
	}
	if !strings.HasPrefix(cmds[1], "wait-for") {
		t.Errorf("cmd[1] = %q, want wait-for prefix", cmds[1])
	}
	if !strings.HasPrefix(cmds[2], "display-message") {
		t.Errorf("cmd[2] = %q, want display-message prefix", cmds[2])
	}
}

func TestExecNonZeroExitCode(t *testing.T) {
	mc := newMockController("%0")
	mc.responses["display-message"] = "127" // command not found

	exec := NewExecutor(mc)

	go func() {
		time.Sleep(5 * time.Millisecond)
		mc.outputCh <- tmux.Notification{PaneID: "%0", Data: "$ cmd\n"}
		mc.outputCh <- tmux.Notification{PaneID: "%0", Data: "bash: cmd: command not found\n"}
		mc.outputCh <- tmux.Notification{PaneID: "%0", Data: "$ "}
	}()

	ctx := context.Background()
	result, err := exec.Exec(ctx, "cmd", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 127 {
		t.Errorf("ExitCode = %d, want 127", result.ExitCode)
	}
}

func TestExecExitCodeParseError(t *testing.T) {
	mc := newMockController("%0")
	mc.responses["display-message"] = "notanumber" // invalid exit code

	exec := NewExecutor(mc)

	go func() {
		time.Sleep(5 * time.Millisecond)
		mc.outputCh <- tmux.Notification{PaneID: "%0", Data: "output\n"}
	}()

	ctx := context.Background()
	_, err := exec.Exec(ctx, "cmd", 5*time.Second)
	if err == nil {
		t.Fatal("expected error for non-numeric exit code")
	}
	if !strings.Contains(err.Error(), "parse exit code") {
		t.Errorf("error = %q, want to contain 'parse exit code'", err.Error())
	}
}

func TestExecNoPaneID(t *testing.T) {
	mc := newMockController("")
	exec := NewExecutor(mc)

	ctx := context.Background()
	_, err := exec.Exec(ctx, "ls", time.Second)
	if err == nil {
		t.Error("expected error with empty pane ID")
	}
}

func TestExecContextTimeout(t *testing.T) {
	mc := newMockController("%0")
	mc.blockPrefixes = []string{"wait-for"} // wait-for blocks until context done

	exec := NewExecutor(mc)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := exec.Exec(ctx, "sleep 100", 0) // no additional timeout
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestRunInit(t *testing.T) {
	mc := newMockController("%0")
	exec := NewExecutor(mc)

	ctx := context.Background()
	if err := exec.RunInit(ctx, "sudo -i"); err != nil {
		t.Fatalf("RunInit: %v", err)
	}

	cmds := mc.getCommands()
	if len(cmds) != 2 {
		t.Fatalf("got %d commands, want 2: %v", len(cmds), cmds)
	}
	if !strings.Contains(cmds[0], "sudo -i") {
		t.Errorf("cmd[0] = %q, want to contain 'sudo -i'", cmds[0])
	}
	if !strings.HasPrefix(cmds[1], "wait-for __sshtmux_wf_") {
		t.Errorf("cmd[1] = %q, want 'wait-for __sshtmux_wf_' prefix", cmds[1])
	}
}

func TestRunInitNoPaneID(t *testing.T) {
	mc := newMockController("")
	exec := NewExecutor(mc)

	ctx := context.Background()
	err := exec.RunInit(ctx, "sudo -i")
	if err == nil {
		t.Error("expected error with empty pane ID")
	}
}

func TestRunInitDrainsOutput(t *testing.T) {
	mc := newMockController("%0")
	exec := NewExecutor(mc)

	// Simulate output from init command
	go func() {
		time.Sleep(5 * time.Millisecond)
		mc.outputCh <- tmux.Notification{PaneID: "%0", Data: "init output\n"}
	}()

	ctx := context.Background()
	if err := exec.RunInit(ctx, "echo init"); err != nil {
		t.Fatalf("RunInit: %v", err)
	}

	// Verify the output channel is drained (no stale output left)
	select {
	case n := <-mc.outputCh:
		t.Errorf("unexpected leftover notification: %+v", n)
	default:
		// good - channel is empty
	}
}

func TestPostProcessOutput(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		channel string
		want    string
	}{
		{
			name:    "strip echo and prompt",
			raw:     "$ ls; __rv=$?; tmux wait-for -S __sshtmux_wf_1\nfile1\nfile2\n$ ",
			channel: "__sshtmux_wf_1",
			want:    "file1\nfile2",
		},
		{
			name:    "multiline echo wrap",
			raw:     "$ ls; __rv=$?; tmux set-option\n -p @sshtmux-rv; tmux wait-for -S __sshtmux_wf_2\nfile1\n$ ",
			channel: "__sshtmux_wf_2",
			want:    "file1",
		},
		{
			name:    "no echo, no prompt",
			raw:     "output\n",
			channel: "nonmatching",
			want:    "output",
		},
		{
			name:    "empty output",
			raw:     "$ cmd; tmux wait-for -S __sshtmux_wf_3\n$ ",
			channel: "__sshtmux_wf_3",
			want:    "",
		},
		{
			name:    "multiline output",
			raw:     "$ cmd; tmux wait-for -S ch1\nline1\nline2\nline3\n$ ",
			channel: "ch1",
			want:    "line1\nline2\nline3",
		},
		{
			name:    "completely empty",
			raw:     "",
			channel: "ch",
			want:    "",
		},
		{
			name:    "ansi codes stripped",
			raw:     "\x1b[?2004l$ cmd; tmux wait-for -S ch2\n\x1b[0moutput\n$ ",
			channel: "ch2",
			want:    "output",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := postProcessOutput(tt.raw, tt.channel)
			if got != tt.want {
				t.Errorf("postProcessOutput() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPostProcessOutputNoTrailingNewline(t *testing.T) {
	// Commands like `printf 'hello'` produce output without a trailing newline.
	// This output should be preserved (not mistakenly stripped as a prompt).
	raw := "$ cmd; tmux wait-for -S ch1\nhello"
	got := postProcessOutput(raw, "ch1")
	if got != "hello" {
		t.Errorf("postProcessOutput() = %q, want %q", got, "hello")
	}
}

func TestPostProcessOutputPromptStripped(t *testing.T) {
	// A standard prompt "$ " at the end should still be stripped
	raw := "$ cmd; tmux wait-for -S ch1\noutput\n$ "
	got := postProcessOutput(raw, "ch1")
	if got != "output" {
		t.Errorf("postProcessOutput() = %q, want %q", got, "output")
	}
}

func TestDrainOutputClosedChannel(t *testing.T) {
	mc := newMockController("%0")
	exec := NewExecutor(mc)

	close(mc.outputCh)

	// Should not hang/loop infinitely
	done := make(chan struct{})
	go func() {
		exec.drainOutput()
		close(done)
	}()

	select {
	case <-done:
		// ok
	case <-time.After(time.Second):
		t.Fatal("drainOutput hung on closed channel")
	}
}

func TestExecLateOutput(t *testing.T) {
	mc := newMockController("%0")
	mc.responses["display-message"] = "0"

	exec := NewExecutor(mc)

	// Simulate output that arrives AFTER wait-for returns.
	go func() {
		time.Sleep(2 * time.Millisecond)
		mc.outputCh <- tmux.Notification{
			PaneID: "%0",
			Data:   "$ set; __rv=$?; tmux set-option -p @sshtmux-rv \"$__rv\"; tmux wait-for -S __sshtmux_wf_1\n",
		}
		// This output arrives 30ms after the command starts.
		time.Sleep(30 * time.Millisecond)
		mc.outputCh <- tmux.Notification{
			PaneID: "%0",
			Data:   "LATE_VAR=late_value\n",
		}
		mc.outputCh <- tmux.Notification{
			PaneID: "%0",
			Data:   "$ ",
		}
	}()

	ctx := context.Background()
	result, err := exec.Exec(ctx, "set", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(result.Output, "LATE_VAR=late_value") {
		t.Errorf("Output = %q, want to contain LATE_VAR=late_value (late output lost)", result.Output)
	}
}

func TestExecMultipleCommands(t *testing.T) {
	mc := newMockController("%0")
	mc.responses["display-message"] = "0"

	exec := NewExecutor(mc)

	for i := range 3 {
		go func(n int) {
			time.Sleep(5 * time.Millisecond)
			mc.outputCh <- tmux.Notification{
				PaneID: "%0",
				Data:   fmt.Sprintf("output-%d\n", n),
			}
		}(i)

		ctx := context.Background()
		result, err := exec.Exec(ctx, fmt.Sprintf("cmd-%d", i), 5*time.Second)
		if err != nil {
			t.Fatalf("Exec[%d]: %v", i, err)
		}
		if result.ExitCode != 0 {
			t.Errorf("Exec[%d] ExitCode = %d, want 0", i, result.ExitCode)
		}
	}
}
