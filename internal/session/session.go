package session

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vlad-zapp/sshtmux/internal/sshclient"
	"github.com/vlad-zapp/sshtmux/internal/tmux"
	"github.com/vlad-zapp/sshtmux/internal/vlog"
)

// Session manages an SSH connection with a tmux control mode session.
type Session struct {
	Host        string
	User        string
	SessionName string

	client   sshclient.Client
	sshSess  sshclient.Session
	ctrl     tmux.Controller
	executor *Executor
}

// Options configures session creation.
type Options struct {
	SessionName    string
	PreCommand     string
	InitCommands   []string
	TmuxSocketPath string
	Term           string
	HistoryLimit   int
}

// startupTimeout bounds the entire tmux startup sequence (handshake, pane
// discovery, init commands) to prevent indefinite hangs when SSH connections
// die silently (e.g. through proxies).
const startupTimeout = 30 * time.Second

// New creates a new session: SSH connect → tmux -C → init commands.
func New(ctx context.Context, dialer sshclient.Dialer, host, user string, opts Options) (*Session, error) {
	vlog.Logf(ctx, "session: creating for host=%q user=%q session_name=%q", host, user, opts.SessionName)
	client, err := dialer.Dial(ctx, host, user)
	if err != nil {
		return nil, fmt.Errorf("ssh dial: %w", err)
	}

	s := &Session{
		Host:        host,
		User:        user,
		SessionName: opts.SessionName,
		client:      client,
	}

	vlog.Logf(ctx, "session: starting tmux (pre_command=%q tmux_socket=%q)", opts.PreCommand, opts.TmuxSocketPath)
	if err := s.startTmux(ctx, opts, true); err != nil {
		// kill-session may have killed the tmux server (last session),
		// which can take down the SSH connection. Reconnect and retry
		// without kill — the old session is already dead.
		vlog.Logf(ctx, "session: first attempt failed (%v), reconnecting without kill", err)
		s.cleanupSSH()

		client, err = dialer.Dial(ctx, host, user)
		if err != nil {
			return nil, fmt.Errorf("ssh redial: %w", err)
		}
		s.client = client

		if err := s.startTmux(ctx, opts, false); err != nil {
			client.Close()
			return nil, fmt.Errorf("start tmux (retry): %w", err)
		}
	}

	return s, nil
}

// cleanupSSH closes the SSH session and client without touching the tmux controller.
func (s *Session) cleanupSSH() {
	if s.sshSess != nil {
		s.sshSess.Close()
		s.sshSess = nil
	}
	if s.client != nil {
		s.client.Close()
		s.client = nil
	}
	s.ctrl = nil
	s.executor = nil
}

// startTmux launches tmux in control mode and sets up the controller.
// When killExisting is true, it kills any prior tmux session on the same socket
// before creating a new one, ensuring clean state. If this causes the SSH
// connection to die (e.g. when the killed session was the last one on the
// server), the caller should reconnect and retry with killExisting=false.
func (s *Session) startTmux(ctx context.Context, opts Options, killExisting bool) error {
	// Apply a startup-specific timeout to prevent indefinite hangs.
	startCtx, startCancel := context.WithTimeout(ctx, startupTimeout)
	defer startCancel()

	preCommand := opts.PreCommand
	tmuxSocketPath := opts.TmuxSocketPath
	sess, err := s.client.NewSession()
	if err != nil {
		return fmt.Errorf("new session: %w", err)
	}
	s.sshSess = sess

	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return fmt.Errorf("stdout pipe: %w", err)
	}

	// Capture SSH stderr so verbose (-v) output and errors are streamed to vlog.
	stderrR, err := sess.StderrPipe()
	if err != nil {
		sess.Close()
		return fmt.Errorf("stderr pipe: %w", err)
	}

	// Build tmux command.
	tmuxBase := "tmux"
	if tmuxSocketPath != "" {
		tmuxBase += " -S " + tmux.ShellQuote(tmuxSocketPath)
	}
	quotedName := tmux.ShellQuote(s.SessionName)
	var tmuxCmd string
	if killExisting {
		// Kill any existing session first to ensure clean state.
		// The old -A (attach-or-create) flag caused stale pane state.
		tmuxCmd = fmt.Sprintf("%s kill-session -t %s 2>/dev/null; %s -C new-session -s %s",
			tmuxBase, quotedName, tmuxBase, quotedName)
	} else {
		tmuxCmd = fmt.Sprintf("%s -C new-session -s %s", tmuxBase, quotedName)
	}

	if preCommand != "" {
		// Pre-command opens a new shell (e.g. "sudo -i"), so we can't use
		// a one-liner. Start SSH in shell mode and send commands via stdin.
		vlog.Logf(ctx, "session: starting ssh in shell mode")
		if err := sess.Start(""); err != nil {
			sess.Close()
			return fmt.Errorf("start ssh shell: %w", err)
		}
	} else {
		vlog.Logf(ctx, "session: starting remote command: %s", tmuxCmd)
		if err := sess.Start(tmuxCmd); err != nil {
			sess.Close()
			return fmt.Errorf("start tmux: %w", err)
		}
	}

	// Stream SSH stderr to vlog immediately so debug output appears in real-time.
	go func() {
		scanner := bufio.NewScanner(stderrR)
		for scanner.Scan() {
			vlog.Logf(ctx, "ssh: %s", scanner.Text())
		}
	}()

	if preCommand != "" {
		vlog.Logf(ctx, "session: sending pre-command: %s", preCommand)
		fmt.Fprintf(stdin, "%s\n", preCommand)

		// Wait for the new shell to produce any output (prompt, MOTD, etc.)
		// This proves the pre-command has executed and the new shell is alive.
		// Pty canonical mode guarantees the tmux command won't be consumed by
		// the parent shell even if it arrives early.
		buf := make([]byte, 4096)
		readDone := make(chan struct{})
		go func() {
			stdout.Read(buf)
			close(readDone)
		}()
		select {
		case <-readDone:
			vlog.Logf(ctx, "session: pre-command ready (stdout data received)")
		case <-startCtx.Done():
			return fmt.Errorf("pre-command readiness: %w", startCtx.Err())
		}

		vlog.Logf(ctx, "session: sending tmux command: %s", tmuxCmd)
		fmt.Fprintf(stdin, "%s\n", tmuxCmd)
	}
	vlog.Logf(ctx, "session: ssh process started, waiting for tmux handshake")

	// Create controller
	ctrl := tmux.NewController(stdout, stdin)
	ctrl.SetLogFunc(func(format string, args ...any) {
		vlog.Logf(ctx, format, args...)
	})
	s.ctrl = ctrl

	// Wait for tmux initial %begin/%end handshake
	if err := ctrl.WaitStartup(startCtx); err != nil {
		return fmt.Errorf("tmux startup: %w", err)
	}
	vlog.Logf(ctx, "session: tmux handshake complete")

	s.executor = NewExecutor(ctrl)

	// Discover pane ID from initial output
	if err := s.discoverPane(startCtx); err != nil {
		return fmt.Errorf("discover pane: %w", err)
	}

	// Discover tmux socket path so pane-embedded commands use an explicit -S flag
	// instead of relying on $TMUX (which may be absent after sudo/toor).
	if sp, err := ctrl.SendCommand(startCtx, "display-message -p '#{socket_path}'"); err == nil {
		val := strings.TrimSpace(sp.Data)
		if val != "" && val[0] == '/' {
			s.executor.SocketPath = val
			vlog.Logf(ctx, "session: discovered socket path %s", val)
		}
	}
	if s.executor.SocketPath == "" && tmuxSocketPath != "" {
		s.executor.SocketPath = tmuxSocketPath
		vlog.Logf(ctx, "session: using configured socket path %s", tmuxSocketPath)
	}

	// Disable mouse mode — it generates escape sequences that interfere
	// with output parsing in control mode.
	if _, err := ctrl.SendCommand(startCtx, "set-option -p mouse off"); err != nil {
		return fmt.Errorf("disable mouse: %w", err)
	}

	// Set scrollback buffer for the pane.
	historyLimit := opts.HistoryLimit
	if historyLimit <= 0 {
		historyLimit = 50000
	}
	ctrl.SendCommand(startCtx, fmt.Sprintf("set-option -p history-limit %d", historyLimit))

	// Wait for shell init scripts to run, then send Ctrl-C to dismiss any
	// prompts they may produce (e.g., SSH host key verification).
	time.Sleep(1 * time.Second)
	ctrl.SendCommand(startCtx, fmt.Sprintf("send-keys -t %s C-c C-c", ctrl.PaneID()))
	time.Sleep(200 * time.Millisecond)
	drainChannel(ctrl.OutputCh())

	// Combine all init commands into a single shell line sent as one send-keys.
	// Sending multiple sequential send-keys can fail because the shell's line
	// editor may flush the pty input buffer between commands, losing keystrokes.
	// A single combined line avoids this entirely.
	term := opts.Term
	if term == "" {
		term = "dumb"
	}
	var initParts []string
	// User init commands first (from config)
	initParts = append(initParts, opts.InitCommands...)
	// Internal setup
	initParts = append(initParts,
		"unset HISTFILE 2>/dev/null",
		"set +o history 2>/dev/null",
		`export PS1=""`,
		fmt.Sprintf("export TERM=%s", term),
	)
	initLine := strings.Join(initParts, "; ")

	vlog.Logf(ctx, "session: running init: %s", initLine)
	if err := s.executor.RunInit(startCtx, initLine); err != nil {
		return fmt.Errorf("init: %w", err)
	}

	return nil
}

// discoverPane reads the initial tmux control mode output to find the pane ID.
func (s *Session) discoverPane(ctx context.Context) error {
	// Use display-message to get the active pane ID
	result, err := s.ctrl.SendCommand(ctx, "display-message -p '#{pane_id}'")
	if err != nil {
		return fmt.Errorf("get pane_id: %w", err)
	}

	paneID := strings.TrimSpace(result.Data)
	if paneID == "" {
		return fmt.Errorf("empty pane_id")
	}
	vlog.Logf(ctx, "session: discovered pane %s", paneID)
	s.ctrl.SetPaneID(paneID)
	return nil
}

// Alive returns true if the session's tmux controller is still running.
func (s *Session) Alive() bool {
	return s.ctrl != nil && s.ctrl.Alive()
}

// Exec executes a command on the remote host.
func (s *Session) Exec(ctx context.Context, command string, timeout time.Duration) (*ExecResult, error) {
	return s.executor.Exec(ctx, command, timeout)
}

// Close cleans up the session, killing the remote tmux session.
func (s *Session) Close() error {
	if s.ctrl != nil {
		// Kill the tmux session so it doesn't persist after disconnect.
		s.ctrl.SendCommand(context.Background(), "kill-session")
		s.ctrl.Close()
	}
	if s.sshSess != nil {
		s.sshSess.Close()
	}
	if s.client != nil {
		return s.client.Close()
	}
	return nil
}

// NewFromController creates a session using an existing controller (for testing).
func NewFromController(ctrl tmux.Controller, host, user string) *Session {
	return &Session{
		Host:     host,
		User:     user,
		ctrl:     ctrl,
		executor: NewExecutor(ctrl),
	}
}
