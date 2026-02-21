package session

import (
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
	if err := s.startTmux(ctx, opts); err != nil {
		client.Close()
		return nil, fmt.Errorf("start tmux: %w", err)
	}

	vlog.Logf(ctx, "session: tmux started, running %d init commands", len(opts.InitCommands))
	// Run init commands
	for _, cmd := range opts.InitCommands {
		vlog.Logf(ctx, "session: init command: %q", cmd)
		if err := s.executor.RunInit(ctx, cmd); err != nil {
			s.Close()
			return nil, fmt.Errorf("init command %q: %w", cmd, err)
		}
	}

	return s, nil
}

// startTmux launches tmux in control mode and sets up the controller.
func (s *Session) startTmux(ctx context.Context, opts Options) error {
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

	// Build tmux command with proper shell quoting for paths
	tmuxCmd := "tmux"
	if tmuxSocketPath != "" {
		tmuxCmd += " -S " + tmux.ShellQuote(tmuxSocketPath)
	}
	tmuxCmd += fmt.Sprintf(" -C new-session -A -s %s", tmux.ShellQuote(s.SessionName))

	if preCommand != "" {
		// Pre-command opens a new shell (e.g. "sudo -i"), so we can't use
		// a one-liner. Start SSH in shell mode and send commands via stdin.
		vlog.Logf(ctx, "session: starting ssh in shell mode")
		if err := sess.Start(""); err != nil {
			sess.Close()
			return fmt.Errorf("start ssh shell: %w", err)
		}

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
		case <-ctx.Done():
			return fmt.Errorf("pre-command readiness: %w", ctx.Err())
		}

		vlog.Logf(ctx, "session: sending tmux command: %s", tmuxCmd)
		fmt.Fprintf(stdin, "%s\n", tmuxCmd)
	} else {
		vlog.Logf(ctx, "session: starting remote command: %s", tmuxCmd)
		if err := sess.Start(tmuxCmd); err != nil {
			sess.Close()
			return fmt.Errorf("start tmux: %w", err)
		}
	}
	vlog.Logf(ctx, "session: ssh process started, waiting for tmux handshake")

	// Create controller
	ctrl := tmux.NewController(stdout, stdin)
	s.ctrl = ctrl
	s.executor = NewExecutor(ctrl)

	// Wait for tmux initial %begin/%end handshake
	if err := ctrl.WaitStartup(ctx); err != nil {
		return fmt.Errorf("tmux startup: %w", err)
	}
	vlog.Logf(ctx, "session: tmux handshake complete")

	// Discover pane ID from initial output
	if err := s.discoverPane(ctx); err != nil {
		return fmt.Errorf("discover pane: %w", err)
	}

	// Disable mouse mode — it generates escape sequences that interfere
	// with output parsing in control mode.
	if _, err := ctrl.SendCommand(ctx, "set-option -p mouse off"); err != nil {
		return fmt.Errorf("disable mouse: %w", err)
	}

	// Set scrollback buffer so capture-pane can retrieve all output.
	historyLimit := opts.HistoryLimit
	if historyLimit <= 0 {
		historyLimit = 50000
	}
	ctrl.SendCommand(ctx, fmt.Sprintf("set-option -p history-limit %d", historyLimit))

	// Disable history first to prevent subsequent commands from being recorded.
	// Set PS1='' (empty prompt) to eliminate prompt artifacts in output.
	// Set TERM to minimize ANSI escape sequences at the source.
	term := opts.Term
	if term == "" {
		term = "dumb"
	}
	initSetup := []struct {
		cmd   string
		fatal bool
	}{
		{"unset HISTFILE", false},
		{"set +o history", false},
		{"export PS1=''", true},
		{fmt.Sprintf("export TERM=%s", term), true},
	}
	for _, init := range initSetup {
		if err := s.executor.RunInit(ctx, init.cmd); err != nil {
			if init.fatal {
				return fmt.Errorf("init %q: %w", init.cmd, err)
			}
		}
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

// Close cleans up the session.
func (s *Session) Close() error {
	if s.ctrl != nil {
		s.ctrl.Detach()
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
