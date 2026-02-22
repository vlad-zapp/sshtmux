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

// echoTail is the suffix we look for in %output to detect the echoed command line.
// Both Exec and RunInit append a completion marker containing this prefix.
const echoTail = "__SSHTMUX_DONE_"

// donePattern matches the completion marker in command output after Enter.
// The captured group is the exit code.
var donePattern = regexp.MustCompile(`__SSHTMUX_DONE_(\d+)__`)

// Executor runs commands via two-phase send with %output streaming.
// Types the command (no Enter) via send-keys -l, consumes the terminal echo,
// presses Enter, then collects output from %output until a completion marker appears.
type Executor struct {
	ctrl    tmux.Controller
	counter atomic.Int64
	sem     chan struct{} // size 1, serializes Exec/RunInit on this session
}

// NewExecutor creates a new executor using the given tmux controller.
func NewExecutor(ctrl tmux.Controller) *Executor {
	return &Executor{ctrl: ctrl, sem: make(chan struct{}, 1)}
}

// Exec executes a command in the tmux pane and returns the output + exit code.
// Uses two-phase send (literal + Enter) with %output streaming for output capture.
//
// Flow: drain stale → send-keys -l → consume echo → send Enter → collect output until marker
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

	outputCh := e.ctrl.OutputCh()
	tag := fmt.Sprintf("exec_%d", e.counter.Add(1))
	vlog.Logf(ctx, "exec: running command=%q tag=%s pane=%s", command, tag, paneID)

	// Drain stale %output.
	drainChannel(outputCh)

	// Type command with completion marker (no Enter).
	shellLine := command + `; __e=$?; printf '\n__SSHTMUX_DONE_%d__\n' "$__e"`
	sendLiteral := tmux.FormatSendKeysLiteral(paneID, shellLine)
	if _, err := e.ctrl.SendCommand(ctx, sendLiteral); err != nil {
		return nil, fmt.Errorf("send-keys literal: %w", err)
	}

	// Consume echo.
	if err := consumeEcho(ctx, outputCh, echoTail); err != nil {
		return nil, fmt.Errorf("consume echo: %w", err)
	}

	// Press Enter.
	sendEnter := tmux.FormatSendKeysEnter(paneID)
	if _, err := e.ctrl.SendCommand(ctx, sendEnter); err != nil {
		return nil, fmt.Errorf("send-keys enter: %w", err)
	}

	// Collect output until completion marker appears.
	var output strings.Builder
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for completion: %w", ctx.Err())
		case data := <-outputCh:
			output.WriteString(data)
			accumulated := output.String()
			if loc := donePattern.FindStringSubmatchIndex(accumulated); loc != nil {
				exitCode, _ := strconv.Atoi(accumulated[loc[2]:loc[3]])
				rawOutput := strings.TrimRight(accumulated[:loc[0]], "\n")
				vlog.Logf(ctx, "exec: done exit_code=%d output_len=%d", exitCode, len(rawOutput))
				return &ExecResult{Output: rawOutput, ExitCode: exitCode}, nil
			}
		}
	}
}

// initTimeout is the per-command timeout for init commands.
const initTimeout = 10 * time.Second

// RunInit executes an init command using two-phase send with streaming.
// Uses the same flow as Exec but discards output and doesn't capture exit code.
//
// Flow: drain stale → send-keys -l → consume echo → send Enter → wait for marker
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

	outputCh := e.ctrl.OutputCh()
	tag := fmt.Sprintf("init_%d", e.counter.Add(1))
	vlog.Logf(ctx, "init: running %q tag=%s pane=%s", command, tag, paneID)

	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()

	drainChannel(outputCh)

	// Type command with completion marker (no Enter).
	shellLine := command + `; printf '\n__SSHTMUX_DONE_0__\n'`
	sendLiteral := tmux.FormatSendKeysLiteral(paneID, shellLine)
	if _, err := e.ctrl.SendCommand(initCtx, sendLiteral); err != nil {
		return fmt.Errorf("send-keys literal: %w", err)
	}

	// Consume echo.
	if err := consumeEcho(initCtx, outputCh, echoTail); err != nil {
		return fmt.Errorf("consume echo: %w", err)
	}

	// Press Enter.
	if _, err := e.ctrl.SendCommand(initCtx, tmux.FormatSendKeysEnter(paneID)); err != nil {
		return fmt.Errorf("send-keys enter: %w", err)
	}

	// Wait for completion marker.
	var buf strings.Builder
	for {
		select {
		case <-initCtx.Done():
			return fmt.Errorf("init timeout: %w", initCtx.Err())
		case data := <-outputCh:
			buf.WriteString(data)
			if strings.Contains(buf.String(), echoTail) {
				drainChannel(outputCh)
				vlog.Logf(ctx, "init: %q done", command)
				return nil
			}
		}
	}
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

// consumeEcho reads from outputCh until the accumulated data contains the tail string.
func consumeEcho(ctx context.Context, ch <-chan string, tail string) error {
	var buf strings.Builder
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case data := <-ch:
			buf.WriteString(data)
			if strings.Contains(buf.String(), tail) {
				return nil
			}
		}
	}
}

// drainChannel discards all currently buffered data from the channel.
func drainChannel(ch <-chan string) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
