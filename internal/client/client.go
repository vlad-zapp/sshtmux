package client

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/vlad-zapp/sshtmux/internal/daemon"
	"github.com/vlad-zapp/sshtmux/internal/vlog"
)

const (
	dialTimeout        = 5 * time.Second
	operationDeadline  = 60 * time.Second
	daemonPollInterval = 100 * time.Millisecond
	daemonPollAttempts = 50 // 50 * 100ms = 5s
)

// Client communicates with the sshtmux daemon over a Unix socket.
type Client struct {
	SocketPath     string
	MaxMessageSize uint32 // 0 means use DefaultMaxMessageSize
}

// NewClient creates a client with the given socket path.
func NewClient(socketPath string) *Client {
	return &Client{SocketPath: socketPath}
}

// Send sends a request to the daemon and returns the response.
// Verbose flag is automatically set based on the current vlog state.
func (c *Client) Send(req daemon.Request) (*daemon.Response, error) {
	req.Verbose = vlog.GetEnabled()
	conn, err := net.DialTimeout("unix", c.SocketPath, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("connect to daemon: %w", err)
	}
	defer conn.Close()

	deadline := operationDeadline
	if req.TimeoutSecs > 0 {
		reqTimeout := time.Duration(req.TimeoutSecs)*time.Second + 10*time.Second
		if reqTimeout > deadline {
			deadline = reqTimeout
		}
	}
	conn.SetDeadline(time.Now().Add(deadline))

	if err := daemon.WriteMessage(conn, &req); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	// Read messages until we get the final (non-streaming) response.
	// Streaming messages contain log lines that are printed immediately.
	for {
		var resp daemon.Response
		if err := daemon.ReadMessage(conn, &resp, c.MaxMessageSize); err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}

		if resp.Streaming {
			fmt.Fprint(os.Stderr, resp.Logs)
			continue
		}

		// Final response — print any remaining logs
		if resp.Logs != "" {
			fmt.Fprint(os.Stderr, resp.Logs)
		}
		return &resp, nil
	}
}

// Exec is a convenience method for executing a command.
// timeoutSecs overrides the default command timeout; 0 means use the daemon default.
func (c *Client) Exec(host, user, command string, timeoutSecs int) (*daemon.Response, error) {
	return c.Send(daemon.Request{
		Type:        "exec",
		Host:        host,
		User:        user,
		Command:     command,
		TimeoutSecs: timeoutSecs,
	})
}

// Disconnect closes a cached connection.
func (c *Client) Disconnect(host, user string) (*daemon.Response, error) {
	return c.Send(daemon.Request{
		Type: "disconnect",
		Host: host,
		User: user,
	})
}

// Status gets active connection status.
func (c *Client) Status() (*daemon.Response, error) {
	return c.Send(daemon.Request{Type: "status"})
}

// Shutdown tells the daemon to stop.
func (c *Client) Shutdown() (*daemon.Response, error) {
	return c.Send(daemon.Request{Type: "shutdown"})
}

// EnsureDaemon starts the daemon if it's not already running.
// Returns nil if the daemon is already running or was started successfully.
func (c *Client) EnsureDaemon() error {
	// Try to connect to see if daemon is running
	conn, err := net.DialTimeout("unix", c.SocketPath, time.Second)
	if err == nil {
		conn.Close()
		return nil
	}

	// Daemon not running - start it
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}

	// Ensure socket directory exists
	if dir := filepath.Dir(c.SocketPath); dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("create socket directory: %w", err)
		}
	}

	cmd := exec.Command(exe, "daemon", "--socket", c.SocketPath)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	// Start as a background process
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	// Release the child process
	cmd.Process.Release()

	// Wait for daemon to become ready
	for range daemonPollAttempts {
		time.Sleep(daemonPollInterval)
		conn, err := net.DialTimeout("unix", c.SocketPath, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
	}

	return fmt.Errorf("daemon did not start within 5 seconds")
}
