package session

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vlad-zapp/sshtmux/internal/tmux"
)

// mockController simulates a tmux controller for testing.
type mockController struct {
	mu       sync.Mutex
	paneID   string
	commands []string
	// responseFunc maps command prefix to a function returning response data.
	// Checked before the static responses map.
	responseFunc map[string]func(cmd string) string
	// responses maps command prefix to response data
	responses map[string]string
	// blockPrefixes: commands matching these prefixes block until context is done
	blockPrefixes []string
	// pipelineCalls tracks number of SendCommandPipeline invocations
	pipelineCalls int
	// outputCh is the channel for streaming %output data
	outputCh chan string
}

func newMockController(paneID string) *mockController {
	return &mockController{
		paneID:       paneID,
		responses:    make(map[string]string),
		responseFunc: make(map[string]func(cmd string) string),
		outputCh:     make(chan string, 1024),
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

	// Check for dynamic response function first
	m.mu.Lock()
	defer m.mu.Unlock()
	for prefix, fn := range m.responseFunc {
		if strings.HasPrefix(cmd, prefix) {
			return &tmux.CommandResult{Data: fn(cmd)}, nil
		}
	}
	// Check for matching static response
	for prefix, data := range m.responses {
		if strings.HasPrefix(cmd, prefix) {
			return &tmux.CommandResult{Data: data}, nil
		}
	}
	return &tmux.CommandResult{}, nil
}

func (m *mockController) SendCommandPipeline(ctx context.Context, cmds []string) ([]*tmux.CommandResult, error) {
	m.mu.Lock()
	m.pipelineCalls++
	m.mu.Unlock()
	results := make([]*tmux.CommandResult, len(cmds))
	for i, cmd := range cmds {
		r, err := m.SendCommand(ctx, cmd)
		if err != nil {
			return nil, err
		}
		results[i] = r
	}
	return results, nil
}

func (m *mockController) OutputCh() <-chan string {
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

// setupExecMock configures the mock for the streaming exec flow.
// Retained for compatibility with session_test.go until Task 6 updates those tests.
// The captureData parameter is ignored in the streaming flow; exitCode configures
// the display-message response for @sshtmux-rv.
func setupExecMock(mc *mockController, captureData string, exitCode int) {
	mc.responseFunc["display-message"] = func(cmd string) string {
		if strings.Contains(cmd, "@sshtmux-done") {
			return "1"
		}
		if strings.Contains(cmd, "@sshtmux-rv") {
			return fmt.Sprintf("%d", exitCode)
		}
		return ""
	}
}

func TestExecStreaming(t *testing.T) {
	mc := newMockController("%0")
	var rvReady atomic.Bool
	mc.responseFunc["display-message"] = func(cmd string) string {
		if strings.Contains(cmd, "@sshtmux-rv") && rvReady.Load() {
			return "0"
		}
		return ""
	}

	exec := NewExecutor(mc, "")

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "ls -la; tmux set-option -p @sshtmux-rv $?"
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "file1.txt\n"
		mc.outputCh <- "file2.txt\n"
		rvReady.Store(true)
	}()

	result, err := exec.Exec(context.Background(), "ls -la", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if !strings.Contains(result.Output, "file1.txt") {
		t.Errorf("Output = %q, want to contain file1.txt", result.Output)
	}
}

func TestExecStreamingNoOutput(t *testing.T) {
	mc := newMockController("%0")
	mc.responseFunc["display-message"] = func(cmd string) string {
		if strings.Contains(cmd, "@sshtmux-rv") {
			return "0"
		}
		return ""
	}

	exec := NewExecutor(mc, "")

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "true; tmux set-option -p @sshtmux-rv $?"
	}()

	result, err := exec.Exec(context.Background(), "true", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Output != "" {
		t.Errorf("Output = %q, want empty", result.Output)
	}
}

func TestExecStreamingEchoInChunks(t *testing.T) {
	mc := newMockController("%0")
	var rvReady atomic.Bool
	mc.responseFunc["display-message"] = func(cmd string) string {
		if strings.Contains(cmd, "@sshtmux-rv") && rvReady.Load() {
			return "0"
		}
		return ""
	}

	exec := NewExecutor(mc, "")

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "echo hello; "
		mc.outputCh <- "tmux set-option"
		mc.outputCh <- " -p @sshtmux-rv $?"
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "hello\n"
		rvReady.Store(true)
	}()

	result, err := exec.Exec(context.Background(), "echo hello", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(result.Output, "hello") {
		t.Errorf("Output = %q, want to contain hello", result.Output)
	}
}

func TestExecStreamingNonZeroExit(t *testing.T) {
	mc := newMockController("%0")
	var rvReady atomic.Bool
	mc.responseFunc["display-message"] = func(cmd string) string {
		if strings.Contains(cmd, "@sshtmux-rv") && rvReady.Load() {
			return "127"
		}
		return ""
	}

	exec := NewExecutor(mc, "")

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "badcmd; tmux set-option -p @sshtmux-rv $?"
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "bash: badcmd: command not found\n"
		rvReady.Store(true)
	}()

	result, err := exec.Exec(context.Background(), "badcmd", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 127 {
		t.Errorf("ExitCode = %d, want 127", result.ExitCode)
	}
}

func TestExecStreamingTimeout(t *testing.T) {
	mc := newMockController("%0")
	mc.responseFunc["display-message"] = func(cmd string) string {
		return "" // rv never set
	}

	exec := NewExecutor(mc, "")

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "sleep 100; tmux set-option -p @sshtmux-rv $?"
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := exec.Exec(ctx, "sleep 100", 0)
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestExecStreamingEchoTimeout(t *testing.T) {
	mc := newMockController("%0")
	exec := NewExecutor(mc, "")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := exec.Exec(ctx, "cmd", 0)
	if err == nil {
		t.Error("expected timeout error when echo never arrives")
	}
}

func TestExecStreamingDrainsStaleOutput(t *testing.T) {
	mc := newMockController("%0")
	var rvReady atomic.Bool
	mc.responseFunc["display-message"] = func(cmd string) string {
		if strings.Contains(cmd, "@sshtmux-rv") && rvReady.Load() {
			return "0"
		}
		return ""
	}

	mc.outputCh <- "stale leftover\n"

	exec := NewExecutor(mc, "")

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "cmd; tmux set-option -p @sshtmux-rv $?"
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "fresh output\n"
		rvReady.Store(true)
	}()

	result, err := exec.Exec(context.Background(), "cmd", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Contains(result.Output, "stale") {
		t.Errorf("Output contains stale data: %q", result.Output)
	}
	if !strings.Contains(result.Output, "fresh output") {
		t.Errorf("Output missing fresh data: %q", result.Output)
	}
}

func TestExecStreamingLargeOutput(t *testing.T) {
	mc := newMockController("%0")
	var rvReady atomic.Bool
	mc.responseFunc["display-message"] = func(cmd string) string {
		if strings.Contains(cmd, "@sshtmux-rv") && rvReady.Load() {
			return "0"
		}
		return ""
	}

	exec := NewExecutor(mc, "")

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "cmd; tmux set-option -p @sshtmux-rv $?"
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "NAMESPACE   NAME        READY\n"
		for i := range 100 {
			mc.outputCh <- fmt.Sprintf("kube-system pod-%d      1/1\n", i)
		}
		rvReady.Store(true)
	}()

	result, err := exec.Exec(context.Background(), "cmd", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(result.Output, "NAMESPACE") {
		t.Errorf("Output missing header")
	}
	if !strings.Contains(result.Output, "pod-99") {
		t.Errorf("Output missing last pod")
	}
}

func TestExecStreamingSocketPath(t *testing.T) {
	mc := newMockController("%0")
	var rvReady atomic.Bool
	mc.responseFunc["display-message"] = func(cmd string) string {
		if strings.Contains(cmd, "@sshtmux-rv") && rvReady.Load() {
			return "0"
		}
		return ""
	}

	exec := NewExecutor(mc, "/tmp/my-socket")

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "echo hello; tmux -S '/tmp/my-socket' set-option -p @sshtmux-rv $?"
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "hello\n"
		rvReady.Store(true)
	}()

	result, err := exec.Exec(context.Background(), "echo hello", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	// Verify the send-keys -l command uses the socket path
	cmds := mc.getCommands()
	var foundSendKeysLiteral bool
	for _, cmd := range cmds {
		if strings.HasPrefix(cmd, "send-keys -l") {
			foundSendKeysLiteral = true
			if !strings.Contains(cmd, "/tmp/my-socket") {
				t.Errorf("send-keys -l should contain socket path: %q", cmd)
			}
		}
	}
	if !foundSendKeysLiteral {
		t.Error("expected send-keys -l command")
	}
}

func TestExecStreamingWithoutSocketPath(t *testing.T) {
	mc := newMockController("%0")
	var rvReady atomic.Bool
	mc.responseFunc["display-message"] = func(cmd string) string {
		if strings.Contains(cmd, "@sshtmux-rv") && rvReady.Load() {
			return "0"
		}
		return ""
	}

	exec := NewExecutor(mc, "")

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "echo hello; tmux set-option -p @sshtmux-rv $?"
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "hello\n"
		rvReady.Store(true)
	}()

	result, err := exec.Exec(context.Background(), "echo hello", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	cmds := mc.getCommands()
	for _, cmd := range cmds {
		if strings.HasPrefix(cmd, "send-keys -l") && strings.Contains(cmd, "-S") {
			t.Errorf("should not contain -S when no socket path: %q", cmd)
		}
	}
	_ = result
}

func TestExecNoPaneID(t *testing.T) {
	mc := newMockController("")
	exec := NewExecutor(mc, "")

	ctx := context.Background()
	_, err := exec.Exec(ctx, "ls", time.Second)
	if err == nil {
		t.Error("expected error with empty pane ID")
	}
}

func TestExecContextTimeout(t *testing.T) {
	mc := newMockController("%0")
	// rv never returns a value
	mc.responseFunc["display-message"] = func(cmd string) string {
		return ""
	}

	exec := NewExecutor(mc, "")

	// Send echo so we get past echo phase
	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "sleep 100; tmux set-option -p @sshtmux-rv $?"
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := exec.Exec(ctx, "sleep 100", 0)
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestExecExitCodeParseError(t *testing.T) {
	mc := newMockController("%0")
	mc.responseFunc["display-message"] = func(cmd string) string {
		if strings.Contains(cmd, "@sshtmux-rv") {
			return "notanumber"
		}
		return ""
	}

	exec := NewExecutor(mc, "")

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "cmd; tmux set-option -p @sshtmux-rv $?"
	}()

	_, err := exec.Exec(context.Background(), "cmd", 5*time.Second)
	if err == nil {
		t.Fatal("expected error for non-numeric exit code")
	}
	if !strings.Contains(err.Error(), "parse exit code") {
		t.Errorf("error = %q, want to contain 'parse exit code'", err.Error())
	}
}

func TestExecConcurrentSemaphoreTimeout(t *testing.T) {
	mc := newMockController("%0")
	// rv never returns a value, so first goroutine blocks on polling
	mc.responseFunc["display-message"] = func(cmd string) string {
		return ""
	}

	exec := NewExecutor(mc, "")

	// First goroutine takes the semaphore and blocks on echo phase (no output sent)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		exec.Exec(ctx, "slow-cmd", 0)
	}()
	time.Sleep(10 * time.Millisecond) // let first goroutine acquire sem

	// Second goroutine should fail with context timeout trying to acquire semaphore
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := exec.Exec(ctx, "fast-cmd", 0)
	if err == nil {
		t.Error("expected timeout error waiting for semaphore")
	}
}

func TestRunInit(t *testing.T) {
	mc := newMockController("%0")
	mc.responseFunc["display-message"] = func(cmd string) string {
		if strings.Contains(cmd, "@sshtmux-done") {
			return "1"
		}
		return ""
	}
	exec := NewExecutor(mc, "")

	ctx := context.Background()
	if err := exec.RunInit(ctx, "sudo -i"); err != nil {
		t.Fatalf("RunInit: %v", err)
	}

	cmds := mc.getCommands()
	// set-option (reset), send-keys, display-message (poll)
	if len(cmds) < 3 {
		t.Fatalf("got %d commands, want at least 3: %v", len(cmds), cmds)
	}
	if !strings.Contains(cmds[0], "@sshtmux-done 0") {
		t.Errorf("cmd[0] = %q, want set-option reset", cmds[0])
	}
	if !strings.Contains(cmds[1], "sudo -i") {
		t.Errorf("cmd[1] = %q, want to contain 'sudo -i'", cmds[1])
	}
	if !strings.Contains(cmds[2], "@sshtmux-done") {
		t.Errorf("cmd[2] = %q, want display-message poll", cmds[2])
	}
}

func TestRunInitNoPaneID(t *testing.T) {
	mc := newMockController("")
	exec := NewExecutor(mc, "")

	ctx := context.Background()
	err := exec.RunInit(ctx, "sudo -i")
	if err == nil {
		t.Error("expected error with empty pane ID")
	}
}

func TestRunInitSendKeysFormat(t *testing.T) {
	mc := newMockController("%0")
	mc.responseFunc["display-message"] = func(cmd string) string {
		if strings.Contains(cmd, "@sshtmux-done") {
			return "1"
		}
		return ""
	}
	exec := NewExecutor(mc, "")

	if err := exec.RunInit(context.Background(), "export FOO=bar"); err != nil {
		t.Fatalf("RunInit: %v", err)
	}

	cmds := mc.getCommands()
	if len(cmds) < 3 {
		t.Fatalf("got %d commands, want at least 3: %v", len(cmds), cmds)
	}
	// send-keys should contain the command and done signal
	sendKeys := cmds[1] // set-option, send-keys, display-message
	if !strings.Contains(sendKeys, "export FOO=bar") {
		t.Errorf("send-keys should contain command: %q", sendKeys)
	}
	if !strings.Contains(sendKeys, "@sshtmux-done 1") {
		t.Errorf("send-keys should contain done signal: %q", sendKeys)
	}
}

func TestRunInitUsesSocketPath(t *testing.T) {
	mc := newMockController("%0")
	mc.responseFunc["display-message"] = func(cmd string) string {
		if strings.Contains(cmd, "@sshtmux-done") {
			return "1"
		}
		return ""
	}
	exec := NewExecutor(mc, "/tmp/my-socket")

	if err := exec.RunInit(context.Background(), "export PS1=''"); err != nil {
		t.Fatalf("RunInit: %v", err)
	}

	cmds := mc.getCommands()
	sendKeys := cmds[1]
	if !strings.Contains(sendKeys, "tmux -S") {
		t.Errorf("send-keys should contain 'tmux -S': %q", sendKeys)
	}
	if !strings.Contains(sendKeys, "/tmp/my-socket") {
		t.Errorf("send-keys should contain socket path: %q", sendKeys)
	}
}

func TestStripANSI(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no ansi", "hello world", "hello world"},
		{"basic color", "\x1b[31mred\x1b[0m", "red"},
		{"bold", "\x1b[1mbold\x1b[0m", "bold"},
		{"cursor move", "\x1b[Htext", "text"},
		{"bracketed paste on", "\x1b[?2004h", ""},
		{"bracketed paste off", "\x1b[?2004l", ""},
		{"osc title", "\x1b]0;window title\x07rest", "rest"},
		{"multiple codes", "\x1b[1m\x1b[31mbold red\x1b[0m", "bold red"},
		{"empty", "", ""},
		{"ansi only", "\x1b[0m\x1b[1m\x1b[31m", ""},
		{"charset selection G0", "\x1b(0line drawing\x1b(B", "line drawing"},
		{"charset selection G1", "\x1b)0text\x1b)B", "text"},
		{"bracketed paste mode set", "\x1b[?2004htext\x1b[?2004l", "text"},
		{"cursor visibility", "\x1b[?25lhidden\x1b[?25h", "hidden"},
		{"mixed csi and charset", "\x1b[1m\x1b(0bold drawing\x1b(B\x1b[0m", "bold drawing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripANSI(tt.input)
			if got != tt.want {
				t.Errorf("StripANSI(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
