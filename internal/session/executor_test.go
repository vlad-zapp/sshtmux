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
	// pipelineCalls tracks number of SendCommandPipeline invocations
	pipelineCalls int
}

func newMockController(paneID string) *mockController {
	return &mockController{
		paneID:    paneID,
		responses: make(map[string]string),
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
	// capture-pane returns the pane content including command echo and output (no prompt with PS1='')
	mc.responses["capture-pane"] = "ls -la; __rv=$?; tmux set-option -p @sshtmux-rv \"$__rv\"; tmux wait-for -S __sshtmux_wf_1\nfile1.txt\nfile2.txt"
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
	mc.responses["capture-pane"] = "cmd; __rv=$?; tmux set-option -p @sshtmux-rv \"$__rv\"; tmux wait-for -S __sshtmux_wf_1\nbash: cmd: command not found"
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
	mc.responses["capture-pane"] = "output"
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
			name:    "strip echo",
			raw:     "ls; __rv=$?; tmux wait-for -S __sshtmux_wf_1\nfile1\nfile2",
			channel: "__sshtmux_wf_1",
			want:    "file1\nfile2",
		},
		{
			name:    "multiline echo wrap",
			raw:     "ls; __rv=$?; tmux set-option\n -p @sshtmux-rv; tmux wait-for -S __sshtmux_wf_2\nfile1",
			channel: "__sshtmux_wf_2",
			want:    "file1",
		},
		{
			name:    "no echo match",
			raw:     "output\n",
			channel: "nonmatching",
			want:    "output",
		},
		{
			name:    "empty output after echo",
			raw:     "cmd; tmux wait-for -S __sshtmux_wf_3\n",
			channel: "__sshtmux_wf_3",
			want:    "",
		},
		{
			name:    "multiline output",
			raw:     "cmd; tmux wait-for -S ch1\nline1\nline2\nline3",
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
			name:    "ansi codes preserved (stripping moved to MCP layer)",
			raw:     "cmd; tmux wait-for -S ch2\n\x1b[0moutput",
			channel: "ch2",
			want:    "\x1b[0moutput",
		},
		{
			name:    "capture-pane with trailing blank lines",
			raw:     "cmd; tmux wait-for -S ch3\noutput\n\n\n",
			channel: "ch3",
			want:    "output",
		},
		{
			name:    "capture-pane with previous content",
			raw:     "old stuff\ncmd; tmux wait-for -S ch4\nfresh output\n\n",
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
	raw := "cmd; tmux wait-for -S ch1\nhello"
	got := postProcessOutput(raw, "ch1")
	if got != "hello" {
		t.Errorf("postProcessOutput() = %q, want %q", got, "hello")
	}
}

func TestExecLargeOutput(t *testing.T) {
	mc := newMockController("%0")

	var lines []string
	lines = append(lines, "cmd; __rv=$?; tmux set-option -p @sshtmux-rv \"$__rv\"; tmux wait-for -S __sshtmux_wf_1")
	lines = append(lines, "NAMESPACE   NAME        READY")
	for i := range 100 {
		lines = append(lines, fmt.Sprintf("kube-system pod-%d      1/1", i))
	}
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
		mc.setResponse("capture-pane", fmt.Sprintf("cmd-%d; tmux wait-for -S %s\noutput-%d", i, channel, i))

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
	// With PS1='', "$ " in output is preserved without ambiguity.
	mc := newMockController("%0")
	mc.responses["capture-pane"] = "cmd; __rv=$?; tmux set-option -p @sshtmux-rv \"$__rv\"; tmux wait-for -S __sshtmux_wf_1\nprice is 100$ per unit\ntotal: 200$ "
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
	mc.responses["capture-pane"] = "cmd; tmux wait-for -S __sshtmux_wf_1\noutput"
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

func TestPostProcessOutputCRLF(t *testing.T) {
	raw := "cmd; tmux wait-for -S ch1\r\noutput line"
	got := postProcessOutput(raw, "ch1")
	if got != "output line" {
		t.Errorf("postProcessOutput() = %q, want %q", got, "output line")
	}
}

func TestPostProcessOutputChannelNameInOutput(t *testing.T) {
	// If the command output contains the channel name,
	// LastIndex finds the last occurrence, potentially losing output.
	raw := "echo __sshtmux_wf_1; __rv=$?; tmux wait-for -S __sshtmux_wf_1\n__sshtmux_wf_1"
	got := postProcessOutput(raw, "__sshtmux_wf_1")
	// LastIndex finds the second "__sshtmux_wf_1" (in the output), so everything
	// before it gets stripped. This is a known fragility.
	if got != "" {
		t.Logf("postProcessOutput with channel in output = %q (known limitation)", got)
	}
}

func TestPostProcessOutputNoChannel(t *testing.T) {
	raw := "line1\nline2\n"
	got := postProcessOutput(raw, "")
	if got != "line1\nline2" {
		t.Errorf("postProcessOutput() = %q, want %q", got, "line1\nline2")
	}
}

func TestExecSendKeysContainsWaitFor(t *testing.T) {
	mc := newMockController("%0")
	mc.responses["capture-pane"] = "cmd; tmux wait-for -S __sshtmux_wf_1\noutput"
	mc.responses["display-message"] = "0"

	exec := NewExecutor(mc)
	_, err := exec.Exec(context.Background(), "echo hello", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	cmds := mc.getCommands()
	// Verify the send-keys command contains the wait-for channel and exit code capture
	sendKeys := cmds[1]
	if !strings.Contains(sendKeys, "__rv=$?") {
		t.Errorf("send-keys should contain exit code capture: %q", sendKeys)
	}
	if !strings.Contains(sendKeys, "tmux set-option -p @sshtmux-rv") {
		t.Errorf("send-keys should contain rv storage: %q", sendKeys)
	}
	if !strings.Contains(sendKeys, "tmux wait-for -S") {
		t.Errorf("send-keys should contain wait-for signal: %q", sendKeys)
	}
}

func TestExecEmptyDisplayMessage(t *testing.T) {
	// When display-message returns empty string, exit code should be 0
	mc := newMockController("%0")
	mc.responses["capture-pane"] = "cmd; tmux wait-for -S __sshtmux_wf_1\n"
	mc.responses["display-message"] = ""

	exec := NewExecutor(mc)
	result, err := exec.Exec(context.Background(), "true", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 for empty display-message", result.ExitCode)
	}
}

func TestExecClearHistoryError(t *testing.T) {
	mc := newMockController("%0")
	mc.blockPrefixes = []string{"clear-history"} // blocks on clear-history

	exec := NewExecutor(mc)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := exec.Exec(ctx, "ls", 0)
	if err == nil {
		t.Error("expected error when clear-history times out")
	}
	if !strings.Contains(err.Error(), "clear-history") {
		t.Errorf("error should mention clear-history: %v", err)
	}
}

func TestExecConcurrentSameSession(t *testing.T) {
	mc := newMockController("%0")
	mc.responses["display-message"] = "0"

	exec := NewExecutor(mc)

	const numWorkers = 5
	var wg sync.WaitGroup
	errors := make(chan error, numWorkers)

	for i := range numWorkers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			channel := fmt.Sprintf("__sshtmux_wf_%d", n+1)
			mc.setResponse("capture-pane", fmt.Sprintf("cmd-%d; tmux wait-for -S %s\noutput-%d", n, channel, n))

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := exec.Exec(ctx, fmt.Sprintf("cmd-%d", n), 0)
			if err != nil {
				errors <- fmt.Errorf("worker %d: %v", n, err)
				return
			}
			if result.ExitCode != 0 {
				errors <- fmt.Errorf("worker %d: exit code %d", n, result.ExitCode)
				return
			}
			errors <- nil
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		if err != nil {
			t.Error(err)
		}
	}

	// Verify commands are serialized: each Exec sends 5 tmux commands,
	// so total should be numWorkers * 5 in groups of 5 (not interleaved)
	cmds := mc.getCommands()
	if len(cmds) != numWorkers*5 {
		t.Fatalf("got %d commands, want %d", len(cmds), numWorkers*5)
	}
	// Check that each group of 5 commands follows the correct order
	for i := 0; i < len(cmds); i += 5 {
		if !strings.HasPrefix(cmds[i], "clear-history") {
			t.Errorf("cmd[%d] = %q, want clear-history prefix", i, cmds[i])
		}
		if !strings.HasPrefix(cmds[i+1], "send-keys") {
			t.Errorf("cmd[%d] = %q, want send-keys prefix", i+1, cmds[i+1])
		}
		if !strings.HasPrefix(cmds[i+2], "wait-for") {
			t.Errorf("cmd[%d] = %q, want wait-for prefix", i+2, cmds[i+2])
		}
		if !strings.HasPrefix(cmds[i+3], "capture-pane") {
			t.Errorf("cmd[%d] = %q, want capture-pane prefix", i+3, cmds[i+3])
		}
		if !strings.HasPrefix(cmds[i+4], "display-message") {
			t.Errorf("cmd[%d] = %q, want display-message prefix", i+4, cmds[i+4])
		}
	}
}

func TestExecConcurrentSemaphoreTimeout(t *testing.T) {
	mc := newMockController("%0")
	mc.responses["display-message"] = "0"
	mc.responses["capture-pane"] = "cmd; tmux wait-for -S __sshtmux_wf_1\noutput"
	mc.blockPrefixes = []string{"wait-for"} // blocks until context done

	exec := NewExecutor(mc)

	// First goroutine takes the semaphore and blocks on wait-for
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

func TestRunInitSendKeysFormat(t *testing.T) {
	mc := newMockController("%0")
	exec := NewExecutor(mc)

	if err := exec.RunInit(context.Background(), "export FOO=bar"); err != nil {
		t.Fatalf("RunInit: %v", err)
	}

	cmds := mc.getCommands()
	if len(cmds) != 2 {
		t.Fatalf("got %d commands, want 2: %v", len(cmds), cmds)
	}
	// send-keys should contain the command followed by wait-for signal
	if !strings.Contains(cmds[0], "export FOO=bar") {
		t.Errorf("cmd[0] should contain command: %q", cmds[0])
	}
	if !strings.Contains(cmds[0], "tmux wait-for -S") {
		t.Errorf("cmd[0] should contain wait-for signal: %q", cmds[0])
	}
}

func TestExecUsesPipeline(t *testing.T) {
	mc := newMockController("%0")
	mc.responses["capture-pane"] = "cmd; tmux wait-for -S __sshtmux_wf_1\noutput"
	mc.responses["display-message"] = "0"

	exec := NewExecutor(mc)
	_, err := exec.Exec(context.Background(), "echo hello", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	mc.mu.Lock()
	calls := mc.pipelineCalls
	mc.mu.Unlock()

	if calls != 1 {
		t.Errorf("pipelineCalls = %d, want 1 (send-keys + wait-for should use pipeline)", calls)
	}
}

func TestRunInitUsesPipeline(t *testing.T) {
	mc := newMockController("%0")
	exec := NewExecutor(mc)

	if err := exec.RunInit(context.Background(), "export PS1=''"); err != nil {
		t.Fatalf("RunInit: %v", err)
	}

	mc.mu.Lock()
	calls := mc.pipelineCalls
	mc.mu.Unlock()

	if calls != 1 {
		t.Errorf("pipelineCalls = %d, want 1 (send-keys + wait-for should use pipeline)", calls)
	}
}
