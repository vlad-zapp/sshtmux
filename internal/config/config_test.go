package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	cfg := Default()
	if cfg.SessionName != "sshtmux" {
		t.Errorf("SessionName = %q, want %q", cfg.SessionName, "sshtmux")
	}
	if cfg.ConnectionTimeout.Duration != 5*time.Minute {
		t.Errorf("ConnectionTimeout = %v, want %v", cfg.ConnectionTimeout.Duration, 5*time.Minute)
	}
	if cfg.CommandTimeout.Duration != 30*time.Second {
		t.Errorf("CommandTimeout = %v, want %v", cfg.CommandTimeout.Duration, 30*time.Second)
	}
	if cfg.Host == nil {
		t.Error("Host map should be initialized")
	}
}

func TestLoadNonexistent(t *testing.T) {
	cfg, err := Load("/nonexistent/path/config.toml")
	if err != nil {
		t.Fatalf("Load nonexistent file should return defaults, got error: %v", err)
	}
	if cfg.SessionName != "sshtmux" {
		t.Errorf("SessionName = %q, want default %q", cfg.SessionName, "sshtmux")
	}
}

func TestLoadFull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
session_name = "mysession"
connection_timeout = "10m"
command_timeout = "1m"
socket_path = "/tmp/test.sock"
tmux_socket_path = "/tmp/tmux.sock"
init_commands = ["sudo -i"]

[host.myserver]
init_commands = ["sudo -u deploy bash"]
session_name = "deploy"
command_timeout = "2m"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SessionName != "mysession" {
		t.Errorf("SessionName = %q, want %q", cfg.SessionName, "mysession")
	}
	if cfg.ConnectionTimeout.Duration != 10*time.Minute {
		t.Errorf("ConnectionTimeout = %v, want %v", cfg.ConnectionTimeout.Duration, 10*time.Minute)
	}
	if cfg.CommandTimeout.Duration != 1*time.Minute {
		t.Errorf("CommandTimeout = %v, want %v", cfg.CommandTimeout.Duration, 1*time.Minute)
	}
	if cfg.SocketPath != "/tmp/test.sock" {
		t.Errorf("SocketPath = %q, want %q", cfg.SocketPath, "/tmp/test.sock")
	}
	if cfg.TmuxSocketPath != "/tmp/tmux.sock" {
		t.Errorf("TmuxSocketPath = %q, want %q", cfg.TmuxSocketPath, "/tmp/tmux.sock")
	}
	if len(cfg.InitCommands) != 1 || cfg.InitCommands[0] != "sudo -i" {
		t.Errorf("InitCommands = %v, want [sudo -i]", cfg.InitCommands)
	}
	hc, ok := cfg.Host["myserver"]
	if !ok {
		t.Fatal("Host[myserver] not found")
	}
	if hc.SessionName != "deploy" {
		t.Errorf("Host.SessionName = %q, want %q", hc.SessionName, "deploy")
	}
	if len(hc.InitCommands) != 1 || hc.InitCommands[0] != "sudo -u deploy bash" {
		t.Errorf("Host.InitCommands = %v, want [sudo -u deploy bash]", hc.InitCommands)
	}
	if hc.CommandTimeout.Duration != 2*time.Minute {
		t.Errorf("Host.CommandTimeout = %v, want %v", hc.CommandTimeout.Duration, 2*time.Minute)
	}
}

func TestLoadInvalidTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(path, []byte("not valid = [toml"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Error("Load invalid TOML should return error")
	}
}

func TestHostSettings_Override(t *testing.T) {
	cfg := Default()
	cfg.InitCommands = []string{"global-init"}
	cfg.Host["myhost"] = HostConfig{
		InitCommands: []string{"host-init"},
		SessionName:  "hostsession",
		CommandTimeout: Duration{2 * time.Minute},
	}

	hs := cfg.HostSettings("myhost")
	if hs.SessionName != "hostsession" {
		t.Errorf("SessionName = %q, want %q", hs.SessionName, "hostsession")
	}
	if len(hs.InitCommands) != 1 || hs.InitCommands[0] != "host-init" {
		t.Errorf("InitCommands = %v, want [host-init]", hs.InitCommands)
	}
	if hs.CommandTimeout.Duration != 2*time.Minute {
		t.Errorf("CommandTimeout = %v, want %v", hs.CommandTimeout.Duration, 2*time.Minute)
	}
}

func TestHostSettings_Fallback(t *testing.T) {
	cfg := Default()
	cfg.InitCommands = []string{"global-init"}
	cfg.SessionName = "globalsession"

	hs := cfg.HostSettings("unknown")
	if hs.SessionName != "globalsession" {
		t.Errorf("SessionName = %q, want %q", hs.SessionName, "globalsession")
	}
	if len(hs.InitCommands) != 1 || hs.InitCommands[0] != "global-init" {
		t.Errorf("InitCommands = %v, want [global-init]", hs.InitCommands)
	}
	if hs.CommandTimeout.Duration != 30*time.Second {
		t.Errorf("CommandTimeout = %v, want %v", hs.CommandTimeout.Duration, 30*time.Second)
	}
}

func TestHostSettings_PartialOverride(t *testing.T) {
	cfg := Default()
	cfg.InitCommands = []string{"global-init"}
	cfg.Host["partial"] = HostConfig{
		SessionName: "customsession",
		// InitCommands and CommandTimeout not set -> should fallback
	}

	hs := cfg.HostSettings("partial")
	if hs.SessionName != "customsession" {
		t.Errorf("SessionName = %q, want %q", hs.SessionName, "customsession")
	}
	if len(hs.InitCommands) != 1 || hs.InitCommands[0] != "global-init" {
		t.Errorf("InitCommands should fallback to global: %v", hs.InitCommands)
	}
	if hs.CommandTimeout.Duration != 30*time.Second {
		t.Errorf("CommandTimeout should fallback to global: %v", hs.CommandTimeout.Duration)
	}
}

func TestHostSettings_ExplicitEmptyInitCommands(t *testing.T) {
	cfg := Default()
	cfg.InitCommands = []string{"global-init"}
	cfg.Host["nocommands"] = HostConfig{
		InitCommands: []string{}, // explicitly empty — should NOT fallback to global
	}

	hs := cfg.HostSettings("nocommands")
	if len(hs.InitCommands) != 0 {
		t.Errorf("InitCommands = %v, want empty (explicit empty should not fallback to global)", hs.InitCommands)
	}
}

func TestDurationUnmarshal(t *testing.T) {
	var d Duration
	if err := d.UnmarshalText([]byte("5m")); err != nil {
		t.Fatalf("UnmarshalText: %v", err)
	}
	if d.Duration != 5*time.Minute {
		t.Errorf("Duration = %v, want %v", d.Duration, 5*time.Minute)
	}
}

func TestDurationUnmarshalInvalid(t *testing.T) {
	var d Duration
	if err := d.UnmarshalText([]byte("notaduration")); err == nil {
		t.Error("UnmarshalText should fail for invalid duration")
	}
}
