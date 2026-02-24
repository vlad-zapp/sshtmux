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
	// rvValue simulates the @sshtmux-rv pane option
	rvValue string
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

	// Handle @sshtmux-rv unset
	if strings.Contains(cmd, "set-option -pu") && strings.Contains(cmd, "@sshtmux-rv") {
		m.mu.Lock()
		m.rvValue = ""
		m.mu.Unlock()
		return &tmux.CommandResult{}, nil
	}

	// Handle @sshtmux-rv poll
	if strings.Contains(cmd, "display-message") && strings.Contains(cmd, "@sshtmux-rv") {
		m.mu.Lock()
		val := m.rvValue
		m.mu.Unlock()
		return &tmux.CommandResult{Data: val}, nil
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

func (m *mockController) setRV(val string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rvValue = val
}

// echoForExec returns the expected echo text for a command in Exec.
func echoForExec(cmd string) string {
	return cmd + "; tmux set-option -p -t %0 @sshtmux-rv $?"
}

// echoForInit returns the expected echo text for a command in RunInit.
func echoForInit(cmd string) string {
	return cmd + "; tmux set-option -p -t %0 @sshtmux-rv 0"
}

func TestExecStreaming(t *testing.T) {
	mc := newMockController("%0")
	exec := NewExecutor(mc)

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- echoForExec("ls -la")
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "file1.txt\n"
		mc.outputCh <- "file2.txt\n"
		mc.setRV("0")
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
	exec := NewExecutor(mc)

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- echoForExec("true")
		time.Sleep(10 * time.Millisecond)
		mc.setRV("0")
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
	exec := NewExecutor(mc)

	go func() {
		time.Sleep(10 * time.Millisecond)
		// Echo arrives in chunks, last chunk contains @sshtmux-rv
		mc.outputCh <- "echo hello; tmux set-opt"
		mc.outputCh <- "ion -p -t %0 "
		mc.outputCh <- "@sshtmux-rv $?"
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "hello\n"
		mc.setRV("0")
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
	exec := NewExecutor(mc)

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- echoForExec("badcmd")
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "bash: badcmd: command not found\n"
		mc.setRV("127")
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
	exec := NewExecutor(mc)

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- echoForExec("sleep 100")
		// rv never set — simulates a command that never completes
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
	exec := NewExecutor(mc)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := exec.Exec(ctx, "cmd", 0)
	if err == nil {
		t.Error("expected timeout error when echo never arrives")
	}
}

func TestExecStreamingDrainsStaleOutput(t *testing.T) {
	mc := newMockController("%0")
	mc.outputCh <- "stale leftover\n"

	exec := NewExecutor(mc)

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- echoForExec("cmd")
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "fresh output\n"
		mc.setRV("0")
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
	exec := NewExecutor(mc)

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- echoForExec("cmd")
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "NAMESPACE   NAME        READY\n"
		for i := range 100 {
			mc.outputCh <- fmt.Sprintf("kube-system pod-%d      1/1\n", i)
		}
		mc.setRV("0")
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
	exec := NewExecutor(mc)

	// Send echo so we get past echo phase, but rv never set
	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- echoForExec("sleep 100")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := exec.Exec(ctx, "sleep 100", 0)
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestExecConcurrentSemaphoreTimeout(t *testing.T) {
	mc := newMockController("%0")
	exec := NewExecutor(mc)

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

func TestExecCommandSequence(t *testing.T) {
	mc := newMockController("%0")
	exec := NewExecutor(mc)

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- echoForExec("echo hello")
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "hello\n"
		mc.setRV("0")
	}()

	result, err := exec.Exec(context.Background(), "echo hello", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}

	// Verify command sequence: unset, send-keys -H, send-keys Enter, display-message
	cmds := mc.getCommands()
	if len(cmds) < 4 {
		t.Fatalf("got %d commands, want at least 4: %v", len(cmds), cmds)
	}
	if !strings.Contains(cmds[0], "set-option -pu") || !strings.Contains(cmds[0], "@sshtmux-rv") {
		t.Errorf("cmd[0] = %q, want set-option -pu @sshtmux-rv", cmds[0])
	}
	if !strings.HasPrefix(cmds[1], "send-keys -H") {
		t.Errorf("cmd[1] = %q, want send-keys -H prefix", cmds[1])
	}
	if !strings.HasPrefix(cmds[2], "send-keys") || !strings.HasSuffix(cmds[2], "Enter") {
		t.Errorf("cmd[2] = %q, want send-keys Enter", cmds[2])
	}
	if !strings.Contains(cmds[3], "display-message") || !strings.Contains(cmds[3], "@sshtmux-rv") {
		t.Errorf("cmd[3] = %q, want display-message poll", cmds[3])
	}
}

func TestExecMultilineCommand(t *testing.T) {
	mc := newMockController("%0")
	exec := NewExecutor(mc)

	multilineCmd := "set -euo pipefail\necho hello"

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- echoForExec(multilineCmd)
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "hello\n"
		mc.setRV("0")
	}()

	result, err := exec.Exec(context.Background(), multilineCmd, 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}

	cmds := mc.getCommands()
	if len(cmds) < 2 {
		t.Fatalf("got %d commands, want at least 2: %v", len(cmds), cmds)
	}
	// send-keys must use -H hex mode
	if !strings.HasPrefix(cmds[1], "send-keys -H") {
		t.Errorf("cmd[1] = %q, want send-keys -H prefix", cmds[1])
	}
	// Newlines must be encoded as Ctrl+V (16) + newline (0a), not bare 0a
	if !strings.Contains(cmds[1], " 16 0a ") {
		t.Errorf("cmd[1] = %q, want Ctrl+V newline (16 0a) for embedded newline", cmds[1])
	}
	// Must NOT contain bare 0a without preceding 16 (which would execute prematurely)
	// The hex string should only have 0a immediately after 16
	hexPart := cmds[1][len("send-keys -H -t %0"):]
	hexTokens := strings.Fields(hexPart)
	for i, tok := range hexTokens {
		if tok == "0a" && (i == 0 || hexTokens[i-1] != "16") {
			t.Errorf("cmd[1] contains bare 0a at position %d without preceding 16: %q", i, cmds[1])
		}
	}
}

func TestRunInitStreaming(t *testing.T) {
	mc := newMockController("%0")
	exec := NewExecutor(mc)

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- echoForInit("sudo -i")
		time.Sleep(10 * time.Millisecond)
		mc.setRV("0")
	}()

	if err := exec.RunInit(context.Background(), "sudo -i"); err != nil {
		t.Fatalf("RunInit: %v", err)
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

func TestRunInitStreamingCommandSequence(t *testing.T) {
	mc := newMockController("%0")
	exec := NewExecutor(mc)

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- echoForInit("export FOO=bar")
		time.Sleep(10 * time.Millisecond)
		mc.setRV("0")
	}()

	if err := exec.RunInit(context.Background(), "export FOO=bar"); err != nil {
		t.Fatalf("RunInit: %v", err)
	}

	cmds := mc.getCommands()
	// Should have: set-option -pu, send-keys -H, send-keys Enter, display-message
	if len(cmds) < 4 {
		t.Fatalf("got %d commands, want at least 4: %v", len(cmds), cmds)
	}
	if !strings.Contains(cmds[0], "set-option -pu") || !strings.Contains(cmds[0], "@sshtmux-rv") {
		t.Errorf("cmd[0] = %q, want set-option -pu @sshtmux-rv", cmds[0])
	}
	if !strings.HasPrefix(cmds[1], "send-keys -H") {
		t.Errorf("cmd[1] = %q, want send-keys -H prefix", cmds[1])
	}
	if !strings.HasPrefix(cmds[2], "send-keys") || !strings.HasSuffix(cmds[2], "Enter") {
		t.Errorf("cmd[2] = %q, want send-keys Enter", cmds[2])
	}
	if !strings.Contains(cmds[3], "display-message") || !strings.Contains(cmds[3], "@sshtmux-rv") {
		t.Errorf("cmd[3] = %q, want display-message poll", cmds[3])
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
