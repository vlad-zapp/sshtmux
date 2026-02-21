package daemon

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Request represents a CLI-to-daemon request.
type Request struct {
	Type    string `json:"type"`    // "exec", "disconnect", "status", "shutdown"
	Host    string `json:"host"`
	User    string `json:"user"`
	Command string `json:"command"`
	Verbose bool   `json:"verbose,omitempty"`
}

// Response represents a daemon-to-CLI response.
type Response struct {
	Success  bool   `json:"success"`
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error"`
	Logs     string `json:"logs,omitempty"`
}

const maxMessageSize = 10 * 1024 * 1024 // 10 MB

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
func ReadMessage(r io.Reader, v any) error {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return fmt.Errorf("read length: %w", err)
	}
	size := binary.BigEndian.Uint32(lenBuf[:])
	if size > maxMessageSize {
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
