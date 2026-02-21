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

func (m *mockController) Alive() bool {
	return true
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

func (m *mockController) setResponse(prefix, data string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses[prefix] = data
}

func TestExecBasic(t *testing.T) {
	mc := newMockController("%0")
	// capture-pane returns the pane content including command echo and output
	mc.responses["capture-pane"] = "$ ls -la; __rv=$?; tmux set-option -p @sshtmux-rv \"$__rv\"; tmux wait-for -S __sshtmux_wf_1\nfile1.txt\nfile2.txt\n$ "
	mc.responses["display-message"] = "0"

	exec := NewExecutor(mc)

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

	// Verify command sequence: clear-history, send-keys, wait-for, capture-pane, display-message
	cmds := mc.getCommands()
	if len(cmds) != 5 {
		t.Fatalf("got %d commands, want 5: %v", len(cmds), cmds)
	}
	if !strings.HasPrefix(cmds[0], "clear-history") {
		t.Errorf("cmd[0] = %q, want clear-history prefix", cmds[0])
	}
	if !strings.HasPrefix(cmds[1], "send-keys") {
		t.Errorf("cmd[1] = %q, want send-keys prefix", cmds[1])
	}
	if !strings.HasPrefix(cmds[2], "wait-for") {
		t.Errorf("cmd[2] = %q, want wait-for prefix", cmds[2])
	}
	if !strings.HasPrefix(cmds[3], "capture-pane") {
		t.Errorf("cmd[3] = %q, want capture-pane prefix", cmds[3])
	}
	if !strings.HasPrefix(cmds[4], "display-message") {
		t.Errorf("cmd[4] = %q, want display-message prefix", cmds[4])
	}
}

func TestExecNonZeroExitCode(t *testing.T) {
	mc := newMockController("%0")
	mc.responses["capture-pane"] = "$ cmd; __rv=$?; tmux set-option -p @sshtmux-rv \"$__rv\"; tmux wait-for -S __sshtmux_wf_1\nbash: cmd: command not found\n$ "
	mc.responses["display-message"] = "127"

	exec := NewExecutor(mc)

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
	mc.responses["capture-pane"] = "output\n$ "
	mc.responses["display-message"] = "notanumber"

	exec := NewExecutor(mc)

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
		{
			name:    "capture-pane with trailing blank lines",
			raw:     "$ cmd; tmux wait-for -S ch3\noutput\n$ \n\n\n",
			channel: "ch3",
			want:    "output",
		},
		{
			name:    "capture-pane with previous content",
			raw:     "old stuff\n$ cmd; tmux wait-for -S ch4\nfresh output\n$ \n\n",
			channel: "ch4",
			want:    "fresh output",
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

func TestExecLargeOutput(t *testing.T) {
	// Simulate large command output (e.g., kubectl get pods -A).
	// With capture-pane, this is completely reliable — no timing issues.
	mc := newMockController("%0")

	var lines []string
	lines = append(lines, "$ cmd; __rv=$?; tmux set-option -p @sshtmux-rv \"$__rv\"; tmux wait-for -S __sshtmux_wf_1")
	lines = append(lines, "NAMESPACE   NAME        READY")
	for i := range 100 {
		lines = append(lines, fmt.Sprintf("kube-system pod-%d      1/1", i))
	}
	lines = append(lines, "$ ")
	mc.responses["capture-pane"] = strings.Join(lines, "\n")
	mc.responses["display-message"] = "0"

	exec := NewExecutor(mc)

	ctx := context.Background()
	result, err := exec.Exec(ctx, "cmd", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(result.Output, "NAMESPACE") {
		t.Errorf("Output missing header")
	}
	if !strings.Contains(result.Output, "pod-99") {
		t.Errorf("Output missing last pod")
	}
	// Count lines
	outputLines := strings.Count(result.Output, "\n") + 1
	if outputLines != 101 {
		t.Errorf("got %d lines, want 101", outputLines)
	}
}

func TestExecMultipleCommands(t *testing.T) {
	mc := newMockController("%0")
	mc.responses["display-message"] = "0"

	exec := NewExecutor(mc)

	for i := range 3 {
		// Set capture-pane response for this command
		// Channel increments: __sshtmux_wf_1, __sshtmux_wf_2, __sshtmux_wf_3
		// But clear-history is cmd 1, so counter is 1 for clear + 1 for the channel per call
		// Actually: counter starts at 0. First Exec: counter.Add(1) = 1, second = 2, third = 3
		channel := fmt.Sprintf("__sshtmux_wf_%d", i+1)
		mc.setResponse("capture-pane", fmt.Sprintf("$ cmd-%d; tmux wait-for -S %s\noutput-%d\n$ ", i, channel, i))

		ctx := context.Background()
		result, err := exec.Exec(ctx, fmt.Sprintf("cmd-%d", i), 5*time.Second)
		if err != nil {
			t.Fatalf("Exec[%d]: %v", i, err)
		}
		if result.ExitCode != 0 {
			t.Errorf("Exec[%d] ExitCode = %d, want 0", i, result.ExitCode)
		}
		if !strings.Contains(result.Output, fmt.Sprintf("output-%d", i)) {
			t.Errorf("Exec[%d] Output = %q, want to contain output-%d", i, result.Output, i)
		}
	}
}

func TestExecCapturePaneContainsDollarSign(t *testing.T) {
	// Verify that "$ " in command output doesn't get stripped
	// (only the actual trailing prompt should be stripped).
	mc := newMockController("%0")
	mc.responses["capture-pane"] = "$ cmd; __rv=$?; tmux set-option -p @sshtmux-rv \"$__rv\"; tmux wait-for -S __sshtmux_wf_1\nprice is 100$ per unit\ntotal: 200$ \n$ "
	mc.responses["display-message"] = "0"

	exec := NewExecutor(mc)

	ctx := context.Background()
	result, err := exec.Exec(ctx, "cmd", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(result.Output, "price is 100$ per unit") {
		t.Errorf("Output missing first line, got %q", result.Output)
	}
	if !strings.Contains(result.Output, "total: 200$ ") {
		t.Errorf("Output missing second line, got %q", result.Output)
	}
}

func TestExecClearHistoryCalledBeforeEachCommand(t *testing.T) {
	mc := newMockController("%0")
	mc.responses["capture-pane"] = "$ cmd; tmux wait-for -S __sshtmux_wf_1\noutput\n$ "
	mc.responses["display-message"] = "0"

	exec := NewExecutor(mc)

	for range 3 {
		ctx := context.Background()
		_, err := exec.Exec(ctx, "cmd", 5*time.Second)
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
	}

	cmds := mc.getCommands()
	// Each Exec sends 5 commands: clear-history, send-keys, wait-for, capture-pane, display-message
	clearCount := 0
	for _, cmd := range cmds {
		if strings.HasPrefix(cmd, "clear-history") {
			clearCount++
		}
	}
	if clearCount != 3 {
		t.Errorf("clear-history called %d times, want 3", clearCount)
	}
}
