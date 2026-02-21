package config

import (
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

type HostConfig struct {
	InitCommands  []string `toml:"init_commands"`
	SessionName   string   `toml:"session_name"`
	CommandTimeout Duration `toml:"command_timeout"`
}

type Config struct {
	SessionName       string                `toml:"session_name"`
	ConnectionTimeout Duration              `toml:"connection_timeout"`
	CommandTimeout    Duration              `toml:"command_timeout"`
	SocketPath        string                `toml:"socket_path"`
	TmuxSocketPath    string                `toml:"tmux_socket_path"`
	InitCommands      []string              `toml:"init_commands"`
	Host              map[string]HostConfig `toml:"host"`
}

// Duration wraps time.Duration for TOML string parsing.
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalText(text []byte) error {
	var err error
	d.Duration, err = time.ParseDuration(string(text))
	return err
}

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.Duration.String()), nil
}

func Default() Config {
	socketPath := filepath.Join(defaultRuntimeDir(), "sshtmux.sock")
	return Config{
		SessionName:       "sshtmux",
		ConnectionTimeout: Duration{5 * time.Minute},
		CommandTimeout:    Duration{30 * time.Second},
		SocketPath:        socketPath,
		Host:              make(map[string]HostConfig),
	}
}

func defaultRuntimeDir() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return dir
	}
	return os.TempDir()
}

func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	if cfg.Host == nil {
		cfg.Host = make(map[string]HostConfig)
	}
	return cfg, nil
}

// DefaultPath returns the default config file path.
func DefaultPath() string {
	if dir, err := os.UserHomeDir(); err == nil {
		return filepath.Join(dir, ".sshtmux.conf")
	}
	return ".sshtmux.conf"
}

// HostSettings returns merged settings for a specific host.
// Host-specific values override global ones when set.
func (c Config) HostSettings(host string) HostConfig {
	hc, ok := c.Host[host]
	if !ok {
		return HostConfig{
			InitCommands:  c.InitCommands,
			SessionName:   c.SessionName,
			CommandTimeout: c.CommandTimeout,
		}
	}
	if hc.SessionName == "" {
		hc.SessionName = c.SessionName
	}
	if hc.CommandTimeout.Duration == 0 {
		hc.CommandTimeout = c.CommandTimeout
	}
	if hc.InitCommands == nil {
		hc.InitCommands = c.InitCommands
	}
	return hc
}
