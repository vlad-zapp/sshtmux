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

const (
	// drainQuietPeriod is how long to wait for additional output after the
	// last notification before considering output collection complete.
	drainQuietPeriod = 50 * time.Millisecond
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
// Uses tmux wait-for for synchronization and pane options for exit code retrieval.
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

	// Drain any stale output notifications before starting
	e.drainOutput()

	channel := fmt.Sprintf("__sshtmux_wf_%d", e.counter.Add(1))
	vlog.Printf("exec: running command=%q channel=%s pane=%s", command, channel, paneID)

	// Start output collector BEFORE send-keys so we capture all notifications
	// including those that arrive during the send-keys %begin/%end processing.
	//
	// Two-phase collection:
	//   Phase 1: Collect output while command runs (between send-keys and wait-for).
	//   Phase 2: After wait-for, drain remaining output until no notifications
	//            arrive for drainQuietPeriod. This handles late %output notifications
	//            that tmux sends asynchronously after processing wait-for.
	collectCtx, collectCancel := context.WithCancel(ctx)
	defer collectCancel()
	waitDone := make(chan struct{})
	outputResult := make(chan string, 1)

	go func() {
		var parts []string
		defer func() {
			outputResult <- strings.Join(parts, "")
		}()

		// Phase 1: Collect output while command runs
		for {
			select {
			case n, ok := <-e.ctrl.OutputChan():
				if !ok {
					return
				}
				if n.PaneID == paneID {
					parts = append(parts, n.Data)
				}
			case <-waitDone:
				goto drain
			case <-collectCtx.Done():
				return
			}
		}

	drain:
		// Phase 2: Drain remaining output until quiet for drainQuietPeriod
		grace := time.NewTimer(drainQuietPeriod)
		defer grace.Stop()
		for {
			select {
			case n, ok := <-e.ctrl.OutputChan():
				if !ok {
					return
				}
				if n.PaneID == paneID {
					parts = append(parts, n.Data)
				}
				if !grace.Stop() {
					select {
					case <-grace.C:
					default:
					}
				}
				grace.Reset(drainQuietPeriod)
			case <-grace.C:
				return
			case <-collectCtx.Done():
				return
			}
		}
	}()

	// Step 1: Send command with wait-for sync suffix
	shellLine := fmt.Sprintf(
		"%s; __rv=$?; tmux set-option -p @sshtmux-rv \"$__rv\"; tmux wait-for -S %s",
		command, channel,
	)
	sendCmd := tmux.FormatSendKeys(paneID, shellLine)
	if _, err := e.ctrl.SendCommand(ctx, sendCmd); err != nil {
		collectCancel()
		<-outputResult
		return nil, fmt.Errorf("send-keys: %w", err)
	}

	vlog.Printf("exec: send-keys done, waiting for completion")
	// Step 2: Block until shell signals via wait-for
	waitCmd := fmt.Sprintf("wait-for %s", channel)
	if _, err := e.ctrl.SendCommand(ctx, waitCmd); err != nil {
		collectCancel()
		<-outputResult
		return nil, fmt.Errorf("wait-for: %w", err)
	}

	// Signal collector to enter drain mode (Phase 2)
	close(waitDone)
	rawOutput := <-outputResult

	// Step 3: Read exit code from pane option
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
	output := postProcessOutput(rawOutput, channel)

	vlog.Printf("exec: done exit_code=%d output_len=%d", exitCode, len(output))
	return &ExecResult{
		Output:   output,
		ExitCode: exitCode,
	}, nil
}

// drainOutput discards any pending output notifications.
func (e *Executor) drainOutput() {
	for {
		select {
		case _, ok := <-e.ctrl.OutputChan():
			if !ok {
				return
			}
		default:
			return
		}
	}
}

// RunInit executes an init command using the wait-for pattern.
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

	// Drain output produced by init command using timer-based drain
	// (same pattern as Exec Phase 2: read until quiet for drainQuietPeriod)
	grace := time.NewTimer(drainQuietPeriod)
	defer grace.Stop()
	for {
		select {
		case _, ok := <-e.ctrl.OutputChan():
			if !ok {
				return nil
			}
			if !grace.Stop() {
				select {
				case <-grace.C:
				default:
				}
			}
			grace.Reset(drainQuietPeriod)
		case <-grace.C:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
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

	// Strip trailing prompt: only strip the last partial line (no trailing newline)
	// if it matches a known prompt pattern. This preserves output from commands
	// like `printf 'hello'` that don't end with a newline.
	if len(raw) > 0 && !strings.HasSuffix(raw, "\n") {
		if idx := strings.LastIndex(raw, "\n"); idx >= 0 {
			lastLine := raw[idx+1:]
			if isPrompt(lastLine) {
				raw = raw[:idx+1]
			}
		} else {
			// Entire output is a single partial line
			if isPrompt(raw) {
				raw = ""
			}
		}
	}

	// Remove trailing newline for clean output
	raw = strings.TrimRight(raw, "\n")

	return raw
}
