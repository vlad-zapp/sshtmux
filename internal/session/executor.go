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
const echoTail = "@sshtmux-rv"

// defaultPollInterval is the default interval for checking command completion.
const defaultPollInterval = 200 * time.Millisecond

// Executor runs commands via two-phase send with %output streaming.
// Types the command (no Enter) via send-keys -l, consumes the terminal echo,
// presses Enter, then polls @sshtmux-rv pane option for completion while
// collecting output from %output.
type Executor struct {
	ctrl         tmux.Controller
	counter      atomic.Int64
	sem          chan struct{}    // size 1, serializes Exec/RunInit on this session
	PollInterval time.Duration   // how often to check @sshtmux-rv; 0 = default (200ms)
	SocketPath   string          // tmux socket path for pane-embedded commands; empty = omit -S
}

// NewExecutor creates a new executor using the given tmux controller.
func NewExecutor(ctrl tmux.Controller) *Executor {
	return &Executor{ctrl: ctrl, sem: make(chan struct{}, 1)}
}

// tmuxCmd returns the tmux command prefix for use inside the pane shell.
func (e *Executor) tmuxCmd() string {
	if e.SocketPath != "" {
		return "tmux -S " + e.SocketPath
	}
	return "tmux"
}

func (e *Executor) pollInterval() time.Duration {
	if e.PollInterval > 0 {
		return e.PollInterval
	}
	return defaultPollInterval
}

// Exec executes a command in the tmux pane and returns the output + exit code.
// Uses two-phase send (literal + Enter) with %output streaming for output capture
// and @sshtmux-rv pane option polling for completion detection.
//
// Flow: drain stale -> unset @sshtmux-rv -> send-keys -l -> consume echo -> send Enter -> poll + collect output
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

	// Unset completion indicator before starting.
	if _, err := e.ctrl.SendCommand(ctx, "set-option -pu -t "+paneID+" @sshtmux-rv"); err != nil {
		return nil, fmt.Errorf("unset @sshtmux-rv: %w", err)
	}

	// Type command with completion signal (no Enter).
	// After the command runs, tmux set-option sets @sshtmux-rv to the exit code.
	shellLine := command + "; " + e.tmuxCmd() + " set-option -p -t " + paneID + " @sshtmux-rv $?"
	sendLiteral := tmux.FormatSendKeysLiteral(paneID, shellLine)
	if _, err := e.ctrl.SendCommand(ctx, sendLiteral); err != nil {
		return nil, fmt.Errorf("send-keys literal: %w", err)
	}

	// Consume echo.
	if err := consumeEcho(ctx, ctx, outputCh, echoTail); err != nil {
		return nil, fmt.Errorf("consume echo: %w", err)
	}

	// Press Enter.
	if _, err := e.ctrl.SendCommand(ctx, tmux.FormatSendKeysEnter(paneID)); err != nil {
		return nil, fmt.Errorf("send-keys enter: %w", err)
	}

	// Poll for completion while collecting output.
	pollCmd := "display-message -p -t " + paneID + " '#{@sshtmux-rv}'"
	ticker := time.NewTicker(e.pollInterval())
	defer ticker.Stop()

	var output strings.Builder
	for {
		select {
		case <-ctx.Done():
			received := output.String()
			if received != "" {
				return nil, fmt.Errorf("waiting for completion (output: %q): %w", received, ctx.Err())
			}
			return nil, fmt.Errorf("waiting for completion (no output received): %w", ctx.Err())
		case data := <-outputCh:
			output.WriteString(data)
		case <-ticker.C:
			result, err := e.ctrl.SendCommand(ctx, pollCmd)
			if err != nil {
				return nil, fmt.Errorf("poll @sshtmux-rv: %w", err)
			}
			val := strings.TrimSpace(result.Data)
			if val != "" {
				exitCode, _ := strconv.Atoi(val)
				// Drain remaining output with a grace period for in-flight data.
				drainWithGrace(outputCh, &output)
				rawOutput := strings.TrimRight(output.String(), "\n")
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
// Flow: drain stale -> unset @sshtmux-rv -> send-keys -l -> consume echo -> send Enter -> poll for completion
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
	vlog.Logf(ctx, "init: running %q tag=%s pane=%s socket=%q", command, tag, paneID, e.SocketPath)

	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()

	drainChannel(outputCh)

	// Unset completion indicator.
	unsetCmd := "set-option -pu -t " + paneID + " @sshtmux-rv"
	vlog.Logf(ctx, "init[%s]: step=unset cmd=%q", tag, unsetCmd)
	if _, err := e.ctrl.SendCommand(initCtx, unsetCmd); err != nil {
		return fmt.Errorf("unset @sshtmux-rv: %w", err)
	}
	vlog.Logf(ctx, "init[%s]: step=unset done", tag)

	// Type command with completion signal (no Enter).
	shellLine := command + "; " + e.tmuxCmd() + " set-option -p -t " + paneID + " @sshtmux-rv 0"
	sendLiteral := tmux.FormatSendKeysLiteral(paneID, shellLine)
	vlog.Logf(ctx, "init[%s]: step=send-keys-l shell_line=%q", tag, shellLine)
	if _, err := e.ctrl.SendCommand(initCtx, sendLiteral); err != nil {
		return fmt.Errorf("send-keys literal: %w", err)
	}
	vlog.Logf(ctx, "init[%s]: step=send-keys-l done, waiting for echo tail=%q", tag, echoTail)

	// Consume echo.
	if err := consumeEcho(ctx, initCtx, outputCh, echoTail); err != nil {
		return fmt.Errorf("consume echo: %w", err)
	}
	vlog.Logf(ctx, "init[%s]: step=echo-consumed", tag)

	// Press Enter.
	if _, err := e.ctrl.SendCommand(initCtx, tmux.FormatSendKeysEnter(paneID)); err != nil {
		return fmt.Errorf("send-keys enter: %w", err)
	}
	vlog.Logf(ctx, "init[%s]: step=enter-sent, polling every %v", tag, e.pollInterval())

	// Poll for completion.
	pollCmd := "display-message -p -t " + paneID + " '#{@sshtmux-rv}'"
	ticker := time.NewTicker(e.pollInterval())
	defer ticker.Stop()

	var buf strings.Builder
	pollCount := 0
	for {
		select {
		case <-initCtx.Done():
			received := buf.String()
			vlog.Logf(ctx, "init[%s]: TIMEOUT after %d polls, output=%q", tag, pollCount, received)
			if received != "" {
				return fmt.Errorf("init timeout (output: %q): %w", received, initCtx.Err())
			}
			return fmt.Errorf("init timeout (no output received): %w", initCtx.Err())
		case data := <-outputCh:
			buf.WriteString(data)
		case <-ticker.C:
			pollCount++
			result, err := e.ctrl.SendCommand(initCtx, pollCmd)
			if err != nil {
				return fmt.Errorf("poll @sshtmux-rv: %w", err)
			}
			val := strings.TrimSpace(result.Data)
			vlog.Logf(ctx, "init[%s]: poll #%d rv=%q", tag, pollCount, val)
			if val != "" {
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
func consumeEcho(logCtx, waitCtx context.Context, ch <-chan string, tail string) error {
	var buf strings.Builder
	for {
		select {
		case <-waitCtx.Done():
			vlog.Logf(logCtx, "consumeEcho: TIMEOUT waiting for %q, accumulated=%q", tail, buf.String())
			return waitCtx.Err()
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

// outputIdleTimeout is how long to wait without new data before considering
// output complete. Resets on each received chunk.
const outputIdleTimeout = 100 * time.Millisecond

// outputHardTimeout is the absolute maximum time to spend draining output
// after command completion, regardless of data flow.
const outputHardTimeout = 30 * time.Second

// drainWithGrace collects remaining output after command completion.
// Waits up to outputIdleTimeout for each new chunk (resets on data),
// with an absolute cap of outputHardTimeout.
func drainWithGrace(ch <-chan string, buf *strings.Builder) {
	hard := time.After(outputHardTimeout)
	idle := time.After(outputIdleTimeout)
	for {
		select {
		case data := <-ch:
			buf.WriteString(data)
			idle = time.After(outputIdleTimeout)
		case <-idle:
			return
		case <-hard:
			return
		}
	}
}
