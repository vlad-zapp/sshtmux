package session

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vlad-zapp/sshtmux/internal/tmux"
	"github.com/vlad-zapp/sshtmux/internal/vlog"
)

// ExecResult holds the result of a command execution.
type ExecResult struct {
	Output   string
	ExitCode int
}

// Executor runs commands via tmux wait-for synchronization.
type Executor struct {
	ctrl       tmux.Controller
	counter    atomic.Int64
	sem        chan struct{} // size 1, serializes Exec/RunInit on this session
	socketPath string       // tmux socket path for -S flag in shell-embedded tmux commands
}

// NewExecutor creates a new executor using the given tmux controller.
// socketPath is the tmux server socket path; when non-empty, all tmux
// commands embedded in shell lines use -S to reach the correct server
// (critical when pre_command spawns a new shell that clears TMUX env var).
func NewExecutor(ctrl tmux.Controller, socketPath string) *Executor {
	e := &Executor{ctrl: ctrl, sem: make(chan struct{}, 1), socketPath: socketPath}
	return e
}

// tmuxCmd returns the tmux command prefix for shell-embedded commands.
// When a socket path is configured, includes -S to ensure the command
// reaches the correct tmux server regardless of environment.
func (e *Executor) tmuxCmd() string {
	if e.socketPath != "" {
		return "tmux -S " + tmux.ShellQuote(e.socketPath)
	}
	return "tmux"
}

// Exec executes a command in the tmux pane and returns the output + exit code.
// Uses tmux wait-for for synchronization and capture-pane for output retrieval.
//
// Flow: clear-history → send-keys → wait-for → capture-pane → display-message
//
// After wait-for returns, the command has finished and all output has been
// written to the pane. capture-pane synchronously retrieves the complete pane
// content via %begin/%end, eliminating all timing issues from %output collection.
func (e *Executor) Exec(ctx context.Context, command string, timeout time.Duration) (*ExecResult, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	paneID := e.ctrl.PaneID()
	if paneID == "" {
		return nil, fmt.Errorf("pane ID not set")
	}

	channel := fmt.Sprintf("__sshtmux_wf_%d", e.counter.Add(1))
	vlog.Logf(ctx, "exec: running command=%q channel=%s pane=%s", command, channel, paneID)

	// Step 1: Clear scrollback so capture-pane returns only this command's output.
	// This prevents unbounded scrollback growth and simplifies output extraction.
	clearCmd := fmt.Sprintf("clear-history -t %s", paneID)
	if _, err := e.ctrl.SendCommand(ctx, clearCmd); err != nil {
		return nil, fmt.Errorf("clear-history: %w", err)
	}

	// Step 2+3: Send command and wait-for in a single pipeline write.
	// This eliminates the SSH round-trip gap between send-keys and wait-for,
	// preventing the shell from signaling before wait-for is registered.
	tmuxBin := e.tmuxCmd()
	shellLine := fmt.Sprintf(
		"%s; __rv=$?; %s set-option -p @sshtmux-rv \"$__rv\"; %s wait-for -S %s",
		command, tmuxBin, tmuxBin, channel,
	)
	sendCmd := tmux.FormatSendKeys(paneID, shellLine)
	waitCmd := fmt.Sprintf("wait-for %s", channel)

	vlog.Logf(ctx, "exec: sending send-keys + wait-for pipeline")
	results, err := e.ctrl.SendCommandPipeline(ctx, []string{sendCmd, waitCmd})
	if err != nil {
		return nil, fmt.Errorf("send-keys+wait-for pipeline: %w", err)
	}
	_ = results // send-keys and wait-for responses are empty on success

	// Step 4: Capture pane content. The command has finished, so all output
	// is in the pane buffer. -p prints to stdout (returned via %begin/%end),
	// -J joins wrapped lines, -S - starts from beginning of scrollback.
	captureCmd := fmt.Sprintf("capture-pane -p -J -S - -t %s", paneID)
	captureResult, err := e.ctrl.SendCommand(ctx, captureCmd)
	if err != nil {
		return nil, fmt.Errorf("capture-pane: %w", err)
	}

	// Step 5: Read exit code from pane option
	displayCmd := fmt.Sprintf("display-message -p -t %s '#{@sshtmux-rv}'", paneID)
	result, err := e.ctrl.SendCommand(ctx, displayCmd)
	if err != nil {
		return nil, fmt.Errorf("display-message: %w", err)
	}

	exitCode := 0
	if result.Data != "" {
		parsed, parseErr := strconv.Atoi(strings.TrimSpace(result.Data))
		if parseErr != nil {
			return nil, fmt.Errorf("parse exit code %q: %w", result.Data, parseErr)
		}
		exitCode = parsed
	}

	// Post-process output: strip echo, prompt, ANSI codes
	output := postProcessOutput(captureResult.Data, channel)

	vlog.Logf(ctx, "exec: done exit_code=%d output_len=%d", exitCode, len(output))
	return &ExecResult{
		Output:   output,
		ExitCode: exitCode,
	}, nil
}

// RunInit executes an init command using the wait-for pattern.
// No output capture is needed for init commands.
func (e *Executor) RunInit(ctx context.Context, command string) error {
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	paneID := e.ctrl.PaneID()
	if paneID == "" {
		return fmt.Errorf("pane ID not set")
	}

	channel := fmt.Sprintf("__sshtmux_wf_%d", e.counter.Add(1))
	vlog.Logf(ctx, "init: running %q channel=%s pane=%s", command, channel, paneID)

	tmuxCmd := e.tmuxCmd()
	shellLine := fmt.Sprintf("%s; %s wait-for -S %s", command, tmuxCmd, channel)
	sendCmd := tmux.FormatSendKeys(paneID, shellLine)
	waitCmd := fmt.Sprintf("wait-for %s", channel)

	vlog.Logf(ctx, "init: sending pipeline (send-keys + wait-for)")
	if _, err := e.ctrl.SendCommandPipeline(ctx, []string{sendCmd, waitCmd}); err != nil {
		return fmt.Errorf("send-keys+wait-for init: %w", err)
	}

	vlog.Logf(ctx, "init: %q done", command)
	return nil
}

// ansiRegex matches ANSI escape sequences:
// - CSI sequences: \x1b[...X (where X is a letter)
// - CSI with ? prefix: \x1b[?...X (bracketed paste mode, etc.)
// - OSC sequences: \x1b]...BEL
// - Character set selection: \x1b(X, \x1b)X
var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x07]*\x07|\x1b[()][0-9A-B]|\x1b\[\?[0-9;]*[hlm]`)

// StripANSI removes ANSI escape sequences from a string.
func StripANSI(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}

// postProcessOutput strips the leading command echo and trailing blank lines.
// The channel parameter is the unique wait-for channel name used to identify the echo boundary.
func postProcessOutput(raw, channel string) string {
	// Normalize line endings: strip \r (terminal outputs \r\n)
	raw = strings.ReplaceAll(raw, "\r", "")

	// Strip leading echo: everything up to and including the LAST line containing
	// the wait-for channel name. Use LastIndex because the echo may span multiple
	// wrapped lines (the terminal re-displays the prompt and rewraps), so the
	// channel name can appear more than once.
	if channel != "" {
		if idx := strings.LastIndex(raw, channel); idx >= 0 {
			// Find the end of the line containing the channel
			rest := raw[idx:]
			if nlIdx := strings.Index(rest, "\n"); nlIdx >= 0 {
				raw = rest[nlIdx+1:]
			} else {
				raw = ""
			}
		}
	}

	// Trim trailing blank lines.
	// capture-pane may include empty rows from the pane area below the cursor.
	raw = strings.TrimRight(raw, "\n")

	return raw
}
