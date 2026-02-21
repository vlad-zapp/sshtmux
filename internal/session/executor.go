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
	ctrl    tmux.Controller
	counter atomic.Int64
}

// NewExecutor creates a new executor using the given tmux controller.
func NewExecutor(ctrl tmux.Controller) *Executor {
	return &Executor{ctrl: ctrl}
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

	paneID := e.ctrl.PaneID()
	if paneID == "" {
		return nil, fmt.Errorf("pane ID not set")
	}

	channel := fmt.Sprintf("__sshtmux_wf_%d", e.counter.Add(1))
	vlog.Printf("exec: running command=%q channel=%s pane=%s", command, channel, paneID)

	// Step 1: Clear scrollback so capture-pane returns only this command's output.
	// This prevents unbounded scrollback growth and simplifies output extraction.
	clearCmd := fmt.Sprintf("clear-history -t %s", paneID)
	if _, err := e.ctrl.SendCommand(ctx, clearCmd); err != nil {
		return nil, fmt.Errorf("clear-history: %w", err)
	}

	// Step 2: Send command with wait-for sync suffix
	shellLine := fmt.Sprintf(
		"%s; __rv=$?; tmux set-option -p @sshtmux-rv \"$__rv\"; tmux wait-for -S %s",
		command, channel,
	)
	sendCmd := tmux.FormatSendKeys(paneID, shellLine)
	if _, err := e.ctrl.SendCommand(ctx, sendCmd); err != nil {
		return nil, fmt.Errorf("send-keys: %w", err)
	}

	vlog.Printf("exec: send-keys done, waiting for completion")
	// Step 3: Block until shell signals via wait-for
	waitCmd := fmt.Sprintf("wait-for %s", channel)
	if _, err := e.ctrl.SendCommand(ctx, waitCmd); err != nil {
		return nil, fmt.Errorf("wait-for: %w", err)
	}

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

	vlog.Printf("exec: done exit_code=%d output_len=%d", exitCode, len(output))
	return &ExecResult{
		Output:   output,
		ExitCode: exitCode,
	}, nil
}

// RunInit executes an init command using the wait-for pattern.
// No output capture is needed for init commands.
func (e *Executor) RunInit(ctx context.Context, command string) error {
	paneID := e.ctrl.PaneID()
	if paneID == "" {
		return fmt.Errorf("pane ID not set")
	}

	channel := fmt.Sprintf("__sshtmux_wf_%d", e.counter.Add(1))

	shellLine := fmt.Sprintf("%s; tmux wait-for -S %s", command, channel)
	sendCmd := tmux.FormatSendKeys(paneID, shellLine)
	if _, err := e.ctrl.SendCommand(ctx, sendCmd); err != nil {
		return fmt.Errorf("send-keys init: %w", err)
	}

	waitCmd := fmt.Sprintf("wait-for %s", channel)
	if _, err := e.ctrl.SendCommand(ctx, waitCmd); err != nil {
		return fmt.Errorf("wait-for init: %w", err)
	}

	return nil
}

// ansiRegex matches ANSI escape sequences.
var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x07]*\x07|\x1b\[\?[0-9;]*[a-zA-Z]`)

// stripANSI removes ANSI escape sequences from a string.
func stripANSI(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}

// isPrompt returns true if the line looks like a standard shell prompt.
func isPrompt(line string) bool {
	return line == "$ " || line == "# " ||
		strings.HasSuffix(line, "$ ") || strings.HasSuffix(line, "# ")
}

// postProcessOutput strips the leading command echo, trailing prompt, and ANSI codes.
// The channel parameter is the unique wait-for channel name used to identify the echo boundary.
// Works with both %output notification data and capture-pane output.
func postProcessOutput(raw, channel string) string {
	// Strip ANSI escape sequences first
	raw = stripANSI(raw)

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

	// Trim trailing blank lines before prompt detection.
	// capture-pane may include empty rows from the pane area below the cursor.
	raw = strings.TrimRight(raw, "\n")

	// Strip trailing prompt if the last line matches a known prompt pattern.
	// This preserves output from commands like `printf 'hello'` that don't
	// end with a newline.
	if len(raw) > 0 {
		if idx := strings.LastIndex(raw, "\n"); idx >= 0 {
			lastLine := raw[idx+1:]
			if isPrompt(lastLine) {
				raw = raw[:idx]
			}
		} else {
			// Entire output is a single partial line
			if isPrompt(raw) {
				raw = ""
			}
		}
	}

	// Final cleanup
	raw = strings.TrimRight(raw, "\n")

	return raw
}
