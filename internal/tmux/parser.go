package tmux

import (
	"fmt"
	"strconv"
	"strings"
)

// MessageType represents the type of a tmux control mode message.
type MessageType int

const (
	MsgBegin  MessageType = iota // %begin <time> <cmd_num> <flags>
	MsgEnd                       // %end <time> <cmd_num> <flags>
	MsgError                     // %error <time> <cmd_num> <flags>
	MsgOutput                    // %output <pane_id> <data>
)

// Message represents a parsed tmux control mode message.
type Message struct {
	Type   MessageType
	CmdNum int    // command number (for begin/end/error)
	Flags  int    // flags field (for begin/end/error)
	PaneID string // pane ID (for output, e.g. "%0")
	Data   string // payload data (output text or error text)
}

// ParseLine parses a single line from tmux control mode output.
// Returns a Message and true if the line is a control message,
// or zero Message and false if it's a data line (part of command response).
func ParseLine(line string) (Message, bool) {
	if !strings.HasPrefix(line, "%") {
		return Message{}, false
	}

	switch {
	case strings.HasPrefix(line, "%begin "):
		return parseBeginEndError(line, MsgBegin)
	case strings.HasPrefix(line, "%end "):
		return parseBeginEndError(line, MsgEnd)
	case strings.HasPrefix(line, "%error "):
		return parseBeginEndError(line, MsgError)
	case strings.HasPrefix(line, "%output "):
		return parseOutput(line)
	default:
		// Unknown % message, treat as notification
		return Message{}, false
	}
}

// parseBeginEndError parses: %<type> <time> <cmd_num> <flags>
func parseBeginEndError(line string, msgType MessageType) (Message, bool) {
	parts := strings.SplitN(line, " ", 4)
	if len(parts) < 4 {
		return Message{}, false
	}
	cmdNum, err := strconv.Atoi(parts[2])
	if err != nil {
		return Message{}, false
	}
	flags, err := strconv.Atoi(parts[3])
	if err != nil {
		return Message{}, false
	}
	return Message{
		Type:   msgType,
		CmdNum: cmdNum,
		Flags:  flags,
	}, true
}

// parseOutput parses: %output <pane_id> <escaped_data>
func parseOutput(line string) (Message, bool) {
	// Format: %output %<id> <data>
	rest := line[len("%output "):]
	spaceIdx := strings.IndexByte(rest, ' ')
	if spaceIdx < 0 {
		// %output with pane but no data
		return Message{
			Type:   MsgOutput,
			PaneID: rest,
		}, true
	}
	paneID := rest[:spaceIdx]
	data := UnescapeOctal(rest[spaceIdx+1:])
	return Message{
		Type:   MsgOutput,
		PaneID: paneID,
		Data:   data,
	}, true
}

// UnescapeOctal converts tmux octal escapes (\0XX) back to bytes.
// tmux control mode escapes non-printable characters and backslash as octal sequences.
func UnescapeOctal(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			// Try to parse 3 octal digits
			o1 := s[i+1]
			o2 := s[i+2]
			o3 := s[i+3]
			if isOctalDigit(o1) && isOctalDigit(o2) && isOctalDigit(o3) {
				val := (o1-'0')*64 + (o2-'0')*8 + (o3-'0')
				b.WriteByte(val)
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctalDigit(c byte) bool {
	return c >= '0' && c <= '7'
}

// FormatSendKeys formats a send-keys command for tmux control mode.
func FormatSendKeys(target, text string) string {
	return fmt.Sprintf("send-keys -t %s %s Enter", target, ShellQuote(text))
}

// ShellQuote quotes a string for tmux/shell command arguments.
func ShellQuote(s string) string {
	// Use single quotes, escaping any existing single quotes
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
