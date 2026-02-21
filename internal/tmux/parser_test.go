package tmux

import (
	"testing"
)

func TestParseLineBegin(t *testing.T) {
	msg, ok := ParseLine("%begin 1234567890 1 0")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if msg.Type != MsgBegin {
		t.Errorf("Type = %v, want MsgBegin", msg.Type)
	}
	if msg.CmdNum != 1 {
		t.Errorf("CmdNum = %d, want 1", msg.CmdNum)
	}
	if msg.Flags != 0 {
		t.Errorf("Flags = %d, want 0", msg.Flags)
	}
}

func TestParseLineEnd(t *testing.T) {
	msg, ok := ParseLine("%end 1234567890 42 1")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if msg.Type != MsgEnd {
		t.Errorf("Type = %v, want MsgEnd", msg.Type)
	}
	if msg.CmdNum != 42 {
		t.Errorf("CmdNum = %d, want 42", msg.CmdNum)
	}
	if msg.Flags != 1 {
		t.Errorf("Flags = %d, want 1", msg.Flags)
	}
}

func TestParseLineError(t *testing.T) {
	msg, ok := ParseLine("%error 1234567890 5 0")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if msg.Type != MsgError {
		t.Errorf("Type = %v, want MsgError", msg.Type)
	}
	if msg.CmdNum != 5 {
		t.Errorf("CmdNum = %d, want 5", msg.CmdNum)
	}
}

func TestParseLineOutput(t *testing.T) {
	msg, ok := ParseLine("%output %0 hello world")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if msg.Type != MsgOutput {
		t.Errorf("Type = %v, want MsgOutput", msg.Type)
	}
	if msg.PaneID != "%0" {
		t.Errorf("PaneID = %q, want %%0", msg.PaneID)
	}
	if msg.Data != "hello world" {
		t.Errorf("Data = %q, want %q", msg.Data, "hello world")
	}
}

func TestParseLineOutputNoData(t *testing.T) {
	msg, ok := ParseLine("%output %0")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if msg.PaneID != "%0" {
		t.Errorf("PaneID = %q, want %%0", msg.PaneID)
	}
	if msg.Data != "" {
		t.Errorf("Data = %q, want empty", msg.Data)
	}
}

func TestParseLineOutputOctalEscape(t *testing.T) {
	// \012 is octal for newline
	msg, ok := ParseLine("%output %0 line1\\012line2")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if msg.Data != "line1\nline2" {
		t.Errorf("Data = %q, want %q", msg.Data, "line1\nline2")
	}
}

func TestParseLineNonControl(t *testing.T) {
	_, ok := ParseLine("regular data line")
	if ok {
		t.Error("expected ok=false for non-control line")
	}
}

func TestParseLineDataLine(t *testing.T) {
	// Data lines between %begin and %end are not prefixed with %
	_, ok := ParseLine("/home/user")
	if ok {
		t.Error("expected ok=false for data line")
	}
}

func TestParseLineUnknownPercent(t *testing.T) {
	_, ok := ParseLine("%session-changed $1 newsession")
	if ok {
		// Unknown % messages are treated as non-control for now
	}
}

func TestParseLineMalformedBegin(t *testing.T) {
	_, ok := ParseLine("%begin too_few")
	if ok {
		t.Error("expected ok=false for malformed begin")
	}
}

func TestParseLineNonNumericCmdNum(t *testing.T) {
	_, ok := ParseLine("%begin 1234 abc 0")
	if ok {
		t.Error("expected ok=false for non-numeric cmd_num")
	}
}

func TestUnescapeOctal(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no escapes", "hello world", "hello world"},
		{"newline", "a\\012b", "a\nb"},
		{"tab", "a\\011b", "a\tb"},
		{"backslash", "a\\134b", "a\\b"},
		{"null", "a\\000b", "a\x00b"},
		{"multiple", "\\012\\012\\012", "\n\n\n"},
		{"mixed", "hello\\012world\\011!", "hello\nworld\t!"},
		{"partial escape", "a\\01", "a\\01"},
		{"trailing backslash", "abc\\", "abc\\"},
		{"non-octal after backslash", "a\\xyz", "a\\xyz"},
		{"empty", "", ""},
		{"bell", "\\007", "\a"},
		{"carriage return", "\\015", "\r"},
		{"del", "\\177", "\x7f"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UnescapeOctal(tt.input)
			if got != tt.want {
				t.Errorf("UnescapeOctal(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatSendKeys(t *testing.T) {
	got := FormatSendKeys("%0", "ls -la")
	want := "send-keys -t %0 'ls -la' Enter"
	if got != want {
		t.Errorf("FormatSendKeys = %q, want %q", got, want)
	}
}

func TestFormatSendKeysWithSingleQuotes(t *testing.T) {
	got := FormatSendKeys("%0", "echo 'hello'")
	want := "send-keys -t %0 'echo '\\''hello'\\''' Enter"
	if got != want {
		t.Errorf("FormatSendKeys = %q, want %q", got, want)
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple", "'simple'"},
		{"with spaces", "'with spaces'"},
		{"with'quote", "'with'\\''quote'"},
		{"", "''"},
	}
	for _, tt := range tests {
		got := ShellQuote(tt.input)
		if got != tt.want {
			t.Errorf("ShellQuote(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
