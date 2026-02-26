package daemon

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestWriteReadRequest(t *testing.T) {
	var buf bytes.Buffer
	req := Request{
		Type:    "exec",
		Host:    "myhost",
		User:    "root",
		Command: "ls -la",
	}
	if err := WriteMessage(&buf, &req); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	var got Request
	if err := ReadMessage(&buf, &got, 0); err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if got != req {
		t.Errorf("got %+v, want %+v", got, req)
	}
}

func TestWriteReadResponse(t *testing.T) {
	var buf bytes.Buffer
	resp := Response{
		Success:  true,
		Output:   "hello world\n",
		ExitCode: 0,
	}
	if err := WriteMessage(&buf, &resp); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	var got Response
	if err := ReadMessage(&buf, &got, 0); err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if got != resp {
		t.Errorf("got %+v, want %+v", got, resp)
	}
}

func TestWriteReadErrorResponse(t *testing.T) {
	var buf bytes.Buffer
	resp := Response{
		Success:  false,
		ExitCode: 1,
		Error:    "command failed",
	}
	if err := WriteMessage(&buf, &resp); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	var got Response
	if err := ReadMessage(&buf, &got, 0); err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if got != resp {
		t.Errorf("got %+v, want %+v", got, resp)
	}
}

func TestReadMessageTruncated(t *testing.T) {
	// Write a valid length header but not enough data
	var buf bytes.Buffer
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], 100)
	buf.Write(lenBuf[:])
	buf.WriteString("short")

	var req Request
	err := ReadMessage(&buf, &req, 0)
	if err == nil {
		t.Error("expected error for truncated message")
	}
}

func TestReadMessageTooLarge(t *testing.T) {
	var buf bytes.Buffer
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], DefaultMaxMessageSize+1)
	buf.Write(lenBuf[:])

	var req Request
	err := ReadMessage(&buf, &req, 0)
	if err == nil {
		t.Error("expected error for oversized message")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error = %q, want to contain 'too large'", err.Error())
	}
}

func TestReadMessageEmptyReader(t *testing.T) {
	var buf bytes.Buffer
	var req Request
	err := ReadMessage(&buf, &req, 0)
	if err == nil {
		t.Error("expected error for empty reader")
	}
}

func TestReadMessageInvalidJSON(t *testing.T) {
	var buf bytes.Buffer
	var lenBuf [4]byte
	data := []byte("not json{")
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	buf.Write(lenBuf[:])
	buf.Write(data)

	var req Request
	err := ReadMessage(&buf, &req, 0)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestMultipleMessagesRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	reqs := []Request{
		{Type: "exec", Host: "h1", Command: "cmd1"},
		{Type: "status"},
		{Type: "disconnect", Host: "h2"},
	}
	for _, r := range reqs {
		if err := WriteMessage(&buf, &r); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}
	}
	for i, want := range reqs {
		var got Request
		if err := ReadMessage(&buf, &got, 0); err != nil {
			t.Fatalf("ReadMessage[%d]: %v", i, err)
		}
		if got != want {
			t.Errorf("[%d] got %+v, want %+v", i, got, want)
		}
	}
}
