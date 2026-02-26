package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vlad-zapp/sshtmux/internal/client"
	"github.com/vlad-zapp/sshtmux/internal/config"
	"github.com/vlad-zapp/sshtmux/internal/daemon"
	"github.com/vlad-zapp/sshtmux/internal/mcpserver"
	"github.com/vlad-zapp/sshtmux/internal/session"
	"github.com/vlad-zapp/sshtmux/internal/sshclient"
	"github.com/vlad-zapp/sshtmux/internal/vlog"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	args := os.Args[1:]

	// Parse -v as first argument
	if args[0] == "-v" {
		vlog.SetEnabled(true)
		args = args[1:]
		if len(args) == 0 {
			printUsage()
			os.Exit(1)
		}
	}

	switch args[0] {
	case "daemon":
		runDaemon(args[1:])
	case "mcp":
		runMCP()
	case "status":
		runStatus()
	case "disconnect":
		runDisconnect(args[1:])
	case "shutdown":
		runShutdown()
	case "help", "--help", "-h":
		printUsage()
	default:
		runExec(args)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage:
  sshtmux [-v] [--timeout duration] [user@]host command   Execute command on remote host
  sshtmux status                     Show cached connections
  sshtmux disconnect [host]          Close cached connection
  sshtmux shutdown                   Stop the daemon
  sshtmux daemon [--socket path]     Run the daemon (auto-started by CLI)
  sshtmux mcp                        Run MCP server (stdio)

Options:
  -v              Verbose output (must be first argument)
  --timeout       Command timeout (e.g. 30s, 1m, 5m). Default: 30s
`)
}

func loadConfig() config.Config {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: config load: %v\n", err)
		return config.Default()
	}
	return cfg
}

func newClient(cfg config.Config) *client.Client {
	c := client.NewClient(cfg.SocketPath)
	c.MaxMessageSize = uint32(cfg.MaxMessageSize)
	return c
}

func runExec(args []string) {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	timeoutFlag := fs.String("timeout", "", "Command timeout (e.g. 30s, 1m, 5m). Default: 30s")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: sshtmux [-v] [--timeout duration] [user@]host command [args...]\n")
	}
	fs.Parse(args)

	remaining := fs.Args()
	if len(remaining) < 2 {
		fs.Usage()
		os.Exit(1)
	}

	host, user := parseHostUser(remaining[0])
	command := strings.Join(remaining[1:], " ")

	var timeoutSecs int
	if *timeoutFlag != "" {
		d, err := time.ParseDuration(*timeoutFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid timeout %q: %v\n", *timeoutFlag, err)
			os.Exit(1)
		}
		timeoutSecs = int(d.Seconds())
		if timeoutSecs <= 0 {
			fmt.Fprintf(os.Stderr, "Error: timeout must be positive\n")
			os.Exit(1)
		}
	}

	vlog.Printf("cli: exec host=%q user=%q command=%q timeout=%d", host, user, command, timeoutSecs)

	cfg := loadConfig()
	c := newClient(cfg)

	vlog.Printf("cli: ensuring daemon is running (socket=%s)", cfg.SocketPath)
	if err := c.EnsureDaemon(); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting daemon: %v\n", err)
		os.Exit(1)
	}

	vlog.Printf("cli: sending exec request to daemon")
	resp, err := c.Exec(host, user, command, timeoutSecs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	vlog.Printf("cli: got response success=%v exit_code=%d error=%q", resp.Success, resp.ExitCode, resp.Error)

	if resp.Error != "" {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp.Error)
		os.Exit(1)
	}

	if resp.Output != "" {
		fmt.Print(resp.Output)
		if !strings.HasSuffix(resp.Output, "\n") {
			fmt.Println()
		}
	}

	os.Exit(resp.ExitCode)
}

func runDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	socketPath := fs.String("socket", "", "Unix socket path")
	verbose := fs.Bool("verbose", false, "Verbose logging")
	fs.Parse(args)

	if *verbose {
		vlog.SetEnabled(true)
	}

	cfg := loadConfig()
	if *socketPath != "" {
		cfg.SocketPath = *socketPath
	}

	vlog.Printf("daemon: starting (socket=%s ignore_host_keys=%v)", cfg.SocketPath, cfg.IgnoreHostKeys)

	dialer := &sshclient.RealDialer{IgnoreHostKeys: cfg.IgnoreHostKeys}
	factory := func(ctx context.Context, host, user string) (*session.Session, error) {
		hs := cfg.HostSettings(host)
		return session.New(ctx, dialer, host, user, session.Options{
			SessionName:    hs.SessionName,
			PreCommand:     hs.PreCommand,
			InitCommands:   hs.InitCommands,
			TmuxSocketPath: cfg.TmuxSocketPath,
			Term:           hs.Term,
			HistoryLimit:   hs.HistoryLimit,
			StartupTimeout: hs.StartupTimeout.Duration,
		})
	}

	pool := daemon.NewConnPool(factory, cfg.ConnectionTimeout.Duration)
	d, err := daemon.NewDaemon(pool, cfg.SocketPath, cfg.CommandTimeout.Duration, uint32(cfg.MaxMessageSize))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	vlog.Printf("daemon: listening on %s", cfg.SocketPath)
	fmt.Fprintf(os.Stderr, "sshtmux daemon listening on %s\n", cfg.SocketPath)
	if err := d.Serve(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runMCP() {
	cfg := loadConfig()
	c := newClient(cfg)

	if err := c.EnsureDaemon(); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting daemon: %v\n", err)
		os.Exit(1)
	}

	srv := mcpserver.New(c)
	if err := srv.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "MCP server error: %v\n", err)
		os.Exit(1)
	}
}

func runStatus() {
	cfg := loadConfig()
	c := newClient(cfg)

	resp, err := c.Status()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if resp.Error != "" {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp.Error)
		os.Exit(1)
	}

	if isEmptyJSONList(resp.Output) {
		fmt.Println("No active connections")
		return
	}

	fmt.Println(resp.Output)
}

func runDisconnect(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: sshtmux disconnect [user@]host\n")
		os.Exit(1)
	}

	host, user := parseHostUser(args[0])
	cfg := loadConfig()
	c := newClient(cfg)

	resp, err := c.Disconnect(host, user)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp.Error)
		os.Exit(1)
	}
	fmt.Println("Disconnected")
}

func runShutdown() {
	cfg := loadConfig()
	c := newClient(cfg)

	resp, err := c.Shutdown()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp.Error)
		os.Exit(1)
	}
	fmt.Println("Daemon shutting down")
}

func parseHostUser(arg string) (host, user string) {
	if idx := strings.LastIndex(arg, "@"); idx >= 0 {
		return arg[idx+1:], arg[:idx]
	}
	return arg, ""
}

// isEmptyJSONList checks if s is a JSON null or an empty JSON array.
func isEmptyJSONList(s string) bool {
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(s), &items); err != nil {
		return s == "null"
	}
	return len(items) == 0
}
