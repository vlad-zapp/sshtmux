# sshtmux

SSH command execution via tmux control mode with persistent connection caching.

sshtmux keeps SSH connections alive inside tmux sessions so repeated commands reuse the same connection instead of reconnecting each time. It runs as a background daemon and exposes both a CLI and an MCP server for AI tool integration.

## Install

Download a binary from [Releases](https://github.com/vlad-zapp/sshtmux/releases) and place it in your `$PATH`.

Or build from source (requires Go 1.24+):

```
go install github.com/vlad-zapp/sshtmux/cmd/sshtmux@latest
```

The remote host must have `tmux` installed.

## Usage

```
# Run a command on a remote host
sshtmux myserver ls -la

# With explicit user
sshtmux deploy@myserver whoami

# Check cached connections
sshtmux status

# Close a specific connection
sshtmux disconnect myserver

# Stop the background daemon
sshtmux shutdown
```

The daemon starts automatically on the first command and caches connections for reuse. Subsequent commands to the same host skip the SSH handshake entirely.

### MCP server

sshtmux includes an MCP server for use with AI tools (Claude Code, etc.):

```
sshtmux mcp
```

This runs on stdio and exposes three tools: `sshtmux_exec`, `sshtmux_disconnect`, and `sshtmux_status`.

## Configuration

Settings are read from `~/.sshtmux.conf` (TOML format). The file is optional -- defaults work out of the box.

### Global settings

```toml
# Tmux session name used on remote hosts (default: "sshtmux")
session_name = "sshtmux"

# How long idle connections stay cached before automatic cleanup (default: "5m")
connection_timeout = "10m"

# Max time a single command can run before being killed (default: "30s")
command_timeout = "1m"

# Unix socket path for daemon communication
# Default: $XDG_RUNTIME_DIR/sshtmux.sock (or $TMPDIR/sshtmux.sock)
socket_path = "/tmp/sshtmux.sock"

# Custom tmux socket path on the remote host (default: tmux's default)
# Useful when the remote user's default tmux socket is inaccessible
tmux_socket_path = "/tmp/my-tmux.sock"

# Commands to run in every new session before accepting commands
# Runs in order, each must complete before the next starts
init_commands = ["sudo -i"]
```

### Per-host overrides

Per-host sections override global settings for specific hosts. Any setting not specified falls back to the global value.

```toml
[host.production]
session_name = "prod"
command_timeout = "2m"
init_commands = ["sudo -u deploy bash"]

[host.staging]
session_name = "staging"
init_commands = ["cd /app && source .env"]

# To explicitly disable global init_commands for a host, set an empty list:
[host.bastion]
init_commands = []
```

Host names match the first argument to `sshtmux` (before `@`), and also match SSH config aliases. For example, if you have `Host prod` in `~/.ssh/config`, use `[host.prod]` in `~/.sshtmux.conf`.

### Full example

```toml
session_name = "sshtmux"
connection_timeout = "10m"
command_timeout = "45s"
init_commands = ["export TERM=dumb"]

[host.webserver]
init_commands = ["cd /var/www && source .env"]
command_timeout = "2m"

[host.database]
session_name = "db-admin"
init_commands = ["sudo -u postgres psql"]
command_timeout = "5m"
```

### Setting reference

| Setting | Type | Default | Description |
|---|---|---|---|
| `session_name` | string | `"sshtmux"` | Tmux session name on the remote host |
| `connection_timeout` | duration | `"5m"` | Idle time before a cached connection is closed |
| `command_timeout` | duration | `"30s"` | Maximum execution time per command |
| `socket_path` | string | `$XDG_RUNTIME_DIR/sshtmux.sock` | Unix socket for daemon IPC |
| `tmux_socket_path` | string | *(tmux default)* | Custom tmux socket on the remote host |
| `init_commands` | string list | *(none)* | Shell commands to run when a session is first created |

Duration values use Go syntax: `"30s"`, `"5m"`, `"1h30m"`, etc.

Per-host sections (`[host.<name>]`) support `session_name`, `command_timeout`, and `init_commands`. Other settings are global only.

## How it works

1. On the first command to a host, sshtmux SSHs in and starts `tmux -C` (control mode)
2. Commands are sent via `send-keys` and synchronized with `wait-for`
3. Output is captured from tmux's `%output` notifications
4. The connection stays cached in the daemon until it expires or is manually disconnected

## License

MIT
