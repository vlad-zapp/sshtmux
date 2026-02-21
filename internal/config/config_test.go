package config

import (
	"os"
	"path/filepath"
	"strings"
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
	if !cfg.IgnoreHostKeys {
		t.Error("IgnoreHostKeys should default to true")
	}
	if cfg.Host == nil {
		t.Error("Host map should be initialized")
	}
	if cfg.Term != "dumb" {
		t.Errorf("Term = %q, want %q", cfg.Term, "dumb")
	}
	if cfg.HistoryLimit != 50000 {
		t.Errorf("HistoryLimit = %d, want %d", cfg.HistoryLimit, 50000)
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

func TestHostSettings_PreCommandFallback(t *testing.T) {
	cfg := Default()
	cfg.PreCommand = "sudo -i"

	hs := cfg.HostSettings("unknown")
	if hs.PreCommand != "sudo -i" {
		t.Errorf("PreCommand = %q, want %q", hs.PreCommand, "sudo -i")
	}
}

func TestHostSettings_PreCommandOverride(t *testing.T) {
	cfg := Default()
	cfg.PreCommand = "sudo -i"
	cfg.Host["myhost"] = HostConfig{
		PreCommand: "sudo -u deploy bash",
	}

	hs := cfg.HostSettings("myhost")
	if hs.PreCommand != "sudo -u deploy bash" {
		t.Errorf("PreCommand = %q, want %q", hs.PreCommand, "sudo -u deploy bash")
	}
}

func TestHostSettings_PreCommandPartialOverride(t *testing.T) {
	cfg := Default()
	cfg.PreCommand = "sudo -i"
	cfg.Host["partial"] = HostConfig{
		SessionName: "custom",
		// PreCommand not set -> should fallback to global
	}

	hs := cfg.HostSettings("partial")
	if hs.PreCommand != "sudo -i" {
		t.Errorf("PreCommand = %q, want %q (should fallback to global)", hs.PreCommand, "sudo -i")
	}
}

func TestLoadPreCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
pre_command = "sudo -i"

[host.myserver]
pre_command = "sudo -u app bash"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PreCommand != "sudo -i" {
		t.Errorf("PreCommand = %q, want %q", cfg.PreCommand, "sudo -i")
	}
	hc, ok := cfg.Host["myserver"]
	if !ok {
		t.Fatal("Host[myserver] not found")
	}
	if hc.PreCommand != "sudo -u app bash" {
		t.Errorf("Host.PreCommand = %q, want %q", hc.PreCommand, "sudo -u app bash")
	}
}

func TestIgnoreHostKeysDefaultTrue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// Config file that does not mention ignore_host_keys
	content := `session_name = "test"`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.IgnoreHostKeys {
		t.Error("IgnoreHostKeys should remain true when not specified in config")
	}
}

func TestIgnoreHostKeysExplicitFalse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `ignore_host_keys = false`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.IgnoreHostKeys {
		t.Error("IgnoreHostKeys should be false when explicitly set to false")
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

func TestDurationMarshalText(t *testing.T) {
	d := Duration{5 * time.Minute}
	text, err := d.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	if string(text) != "5m0s" {
		t.Errorf("MarshalText = %q, want %q", string(text), "5m0s")
	}
}

func TestDefaultPath(t *testing.T) {
	path := DefaultPath()
	if path == "" {
		t.Error("DefaultPath should not be empty")
	}
	if !strings.HasSuffix(path, ".sshtmux.conf") {
		t.Errorf("DefaultPath = %q, want suffix .sshtmux.conf", path)
	}
}

func TestDefaultRuntimeDirWithXDG(t *testing.T) {
	// Set XDG_RUNTIME_DIR and verify it's used
	old := os.Getenv("XDG_RUNTIME_DIR")
	os.Setenv("XDG_RUNTIME_DIR", "/custom/runtime")
	defer os.Setenv("XDG_RUNTIME_DIR", old)

	dir := defaultRuntimeDir()
	if dir != "/custom/runtime" {
		t.Errorf("defaultRuntimeDir = %q, want %q", dir, "/custom/runtime")
	}
}

func TestDefaultRuntimeDirWithoutXDG(t *testing.T) {
	old := os.Getenv("XDG_RUNTIME_DIR")
	os.Unsetenv("XDG_RUNTIME_DIR")
	defer os.Setenv("XDG_RUNTIME_DIR", old)

	dir := defaultRuntimeDir()
	if dir != os.TempDir() {
		t.Errorf("defaultRuntimeDir = %q, want %q", dir, os.TempDir())
	}
}

func TestDefaultSocketPathUsesRuntimeDir(t *testing.T) {
	old := os.Getenv("XDG_RUNTIME_DIR")
	os.Setenv("XDG_RUNTIME_DIR", "/tmp/test-runtime")
	defer os.Setenv("XDG_RUNTIME_DIR", old)

	cfg := Default()
	if cfg.SocketPath != "/tmp/test-runtime/sshtmux.sock" {
		t.Errorf("SocketPath = %q, want %q", cfg.SocketPath, "/tmp/test-runtime/sshtmux.sock")
	}
}

func TestLoadReadError(t *testing.T) {
	// A path that exists but is a directory (not a file) should return an error
	dir := t.TempDir()
	_, err := Load(filepath.Join(dir, "subdir"))
	// /nonexistent subdir doesn't exist -> returns defaults (IsNotExist)
	if err != nil {
		t.Errorf("Load non-existent subpath should return defaults: %v", err)
	}
}

func TestHostSettingsHostMapNil(t *testing.T) {
	cfg := Default()
	cfg.Host = nil
	// HostSettings should handle nil Host map gracefully
	hs := cfg.HostSettings("anyhost")
	if hs.SessionName != cfg.SessionName {
		t.Errorf("SessionName = %q, want %q", hs.SessionName, cfg.SessionName)
	}
}

func TestLoadTermAndHistoryLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
term = "xterm-256color"
history_limit = 100000

[host.myserver]
term = "vt100"
history_limit = 10000
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Term != "xterm-256color" {
		t.Errorf("Term = %q, want %q", cfg.Term, "xterm-256color")
	}
	if cfg.HistoryLimit != 100000 {
		t.Errorf("HistoryLimit = %d, want %d", cfg.HistoryLimit, 100000)
	}
	hc, ok := cfg.Host["myserver"]
	if !ok {
		t.Fatal("Host[myserver] not found")
	}
	if hc.Term != "vt100" {
		t.Errorf("Host.Term = %q, want %q", hc.Term, "vt100")
	}
	if hc.HistoryLimit != 10000 {
		t.Errorf("Host.HistoryLimit = %d, want %d", hc.HistoryLimit, 10000)
	}
}

func TestHostSettings_TermAndHistoryLimitFallback(t *testing.T) {
	cfg := Default()
	cfg.Term = "xterm"
	cfg.HistoryLimit = 80000

	hs := cfg.HostSettings("unknown")
	if hs.Term != "xterm" {
		t.Errorf("Term = %q, want %q", hs.Term, "xterm")
	}
	if hs.HistoryLimit != 80000 {
		t.Errorf("HistoryLimit = %d, want %d", hs.HistoryLimit, 80000)
	}
}

func TestHostSettings_TermAndHistoryLimitOverride(t *testing.T) {
	cfg := Default()
	cfg.Term = "dumb"
	cfg.HistoryLimit = 50000
	cfg.Host["myhost"] = HostConfig{
		Term:         "vt100",
		HistoryLimit: 10000,
	}

	hs := cfg.HostSettings("myhost")
	if hs.Term != "vt100" {
		t.Errorf("Term = %q, want %q", hs.Term, "vt100")
	}
	if hs.HistoryLimit != 10000 {
		t.Errorf("HistoryLimit = %d, want %d", hs.HistoryLimit, 10000)
	}
}

func TestHostSettings_TermAndHistoryLimitPartialOverride(t *testing.T) {
	cfg := Default()
	cfg.Term = "xterm"
	cfg.HistoryLimit = 80000
	cfg.Host["partial"] = HostConfig{
		SessionName: "custom",
		// Term and HistoryLimit not set -> should fallback
	}

	hs := cfg.HostSettings("partial")
	if hs.Term != "xterm" {
		t.Errorf("Term = %q, want %q (should fallback to global)", hs.Term, "xterm")
	}
	if hs.HistoryLimit != 80000 {
		t.Errorf("HistoryLimit = %d, want %d (should fallback to global)", hs.HistoryLimit, 80000)
	}
}
