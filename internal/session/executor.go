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

// pollInterval is how often we check whether a command has finished.
const pollInterval = 200 * time.Millisecond

// echoTail is the suffix we look for in %output to detect the echoed command line.
// Uses "set-option -p @sshtmux-rv $?" without "tmux" prefix so it matches both
// "tmux set-option ..." and "tmux -S '/path' set-option ..." forms.
const echoTail = "set-option -p @sshtmux-rv $?"

// Executor runs commands via tmux pane option polling for synchronization.
// Instead of wait-for (which blocks the control connection and breaks some
// tmux versions), we set a pane option when the command finishes and poll
// for it via display-message.
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
// Uses two-phase send (literal + Enter) with %output streaming for output capture.
//
// Flow: reset rv → drain stale → send-keys -l → consume echo → send Enter → collect output + poll rv
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

	// Step 1: Reset rv.
	resetCmd := fmt.Sprintf("set-option -p -t %s @sshtmux-rv ''", paneID)
	if _, err := e.ctrl.SendCommand(ctx, resetCmd); err != nil {
		return nil, fmt.Errorf("reset rv: %w", err)
	}

	// Drain stale %output.
	drainChannel(outputCh)

	// Step 2: Type command (no Enter).
	tmuxBin := e.tmuxCmd()
	shellLine := fmt.Sprintf("%s; %s set-option -p @sshtmux-rv $?", command, tmuxBin)
	sendLiteral := tmux.FormatSendKeysLiteral(paneID, shellLine)
	if _, err := e.ctrl.SendCommand(ctx, sendLiteral); err != nil {
		return nil, fmt.Errorf("send-keys literal: %w", err)
	}

	// Step 3: Consume echo.
	if err := consumeEcho(ctx, outputCh, echoTail); err != nil {
		return nil, fmt.Errorf("consume echo: %w", err)
	}

	// Step 4: Press Enter.
	sendEnter := tmux.FormatSendKeysEnter(paneID)
	if _, err := e.ctrl.SendCommand(ctx, sendEnter); err != nil {
		return nil, fmt.Errorf("send-keys enter: %w", err)
	}

	// Step 5: Collect output while polling rv.
	displayCmd := fmt.Sprintf("display-message -p -t %s '#{@sshtmux-rv}'", paneID)
	var output strings.Builder

	for {
		result, err := e.ctrl.SendCommand(ctx, displayCmd)
		if err != nil {
			return nil, fmt.Errorf("poll rv: %w", err)
		}
		rv := strings.TrimSpace(result.Data)
		if rv != "" {
			drainOutput(outputCh, &output)

			exitCode, parseErr := strconv.Atoi(rv)
			if parseErr != nil {
				return nil, fmt.Errorf("parse exit code %q: %w", rv, parseErr)
			}

			out := strings.TrimRight(output.String(), "\n")
			vlog.Logf(ctx, "exec: done exit_code=%d output_len=%d", exitCode, len(out))
			return &ExecResult{Output: out, ExitCode: exitCode}, nil
		}

		drainOutput(outputCh, &output)

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("poll rv: %w", ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// initTimeout is the per-command timeout for init commands.
const initTimeout = 10 * time.Second

// RunInit executes an init command using two-phase send with streaming.
// Uses the same flow as Exec but discards all output.
//
// Flow: reset rv → drain stale → send-keys -l → consume echo → send Enter → poll rv
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

	// Reset rv.
	if _, err := e.ctrl.SendCommand(ctx, fmt.Sprintf("set-option -p -t %s @sshtmux-rv ''", paneID)); err != nil {
		return fmt.Errorf("reset rv: %w", err)
	}

	drainChannel(outputCh)

	// Type command (no Enter).
	tmuxBin := e.tmuxCmd()
	shellLine := fmt.Sprintf("%s; %s set-option -p @sshtmux-rv $?", command, tmuxBin)
	sendLiteral := tmux.FormatSendKeysLiteral(paneID, shellLine)
	if _, err := e.ctrl.SendCommand(ctx, sendLiteral); err != nil {
		return fmt.Errorf("send-keys literal: %w", err)
	}

	// Consume echo with init timeout.
	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()

	if err := consumeEcho(initCtx, outputCh, echoTail); err != nil {
		return fmt.Errorf("consume echo: %w", err)
	}

	// Press Enter.
	if _, err := e.ctrl.SendCommand(ctx, tmux.FormatSendKeysEnter(paneID)); err != nil {
		return fmt.Errorf("send-keys enter: %w", err)
	}

	// Poll rv until non-empty.
	displayCmd := fmt.Sprintf("display-message -p -t %s '#{@sshtmux-rv}'", paneID)
	for {
		result, err := e.ctrl.SendCommand(initCtx, displayCmd)
		if err != nil {
			return fmt.Errorf("poll init rv: %w", err)
		}
		if strings.TrimSpace(result.Data) != "" {
			drainChannel(outputCh)
			vlog.Logf(ctx, "init: %q done", command)
			return nil
		}
		drainChannel(outputCh)
		select {
		case <-initCtx.Done():
			return fmt.Errorf("init timeout: %w", initCtx.Err())
		case <-time.After(pollInterval):
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

// drainOutput reads all currently available data from ch into the builder.
func drainOutput(ch <-chan string, buf *strings.Builder) {
	for {
		select {
		case data := <-ch:
			buf.WriteString(data)
		default:
			return
		}
	}
}
