package daemon

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Request represents a CLI-to-daemon request.
type Request struct {
	Type        string `json:"type"`    // "exec", "disconnect", "status", "shutdown"
	Host        string `json:"host"`
	User        string `json:"user"`
	Command     string `json:"command"`
	Verbose     bool   `json:"verbose,omitempty"`
	TimeoutSecs int    `json:"timeout_secs,omitempty"`
}

// Response represents a daemon-to-CLI response.
// When Streaming is true, it's an intermediate log message (Logs field only).
// When Streaming is false, it's the final response.
type Response struct {
	Streaming bool   `json:"streaming,omitempty"`
	Success   bool   `json:"success"`
	Output    string `json:"output"`
	ExitCode  int    `json:"exit_code"`
	Error     string `json:"error"`
	Logs      string `json:"logs,omitempty"`
}

// DefaultMaxMessageSize is the default limit for IPC messages (100 MB).
const DefaultMaxMessageSize = 100 * 1024 * 1024

// WriteMessage writes a length-prefixed JSON message to the writer.
func WriteMessage(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return fmt.Errorf("write length: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write payload: %w", err)
	}
	return nil
}

// ReadMessage reads a length-prefixed JSON message from the reader.
// maxSize limits the message size; 0 means use DefaultMaxMessageSize.
func ReadMessage(r io.Reader, v any, maxSize uint32) error {
	if maxSize == 0 {
		maxSize = DefaultMaxMessageSize
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return fmt.Errorf("read length: %w", err)
	}
	size := binary.BigEndian.Uint32(lenBuf[:])
	if size > maxSize {
		return fmt.Errorf("message too large: %d bytes", size)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return fmt.Errorf("read payload: %w", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}
	return nil
}
