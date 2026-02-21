package vlog

import (
	"bytes"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// enabled controls whether verbose log messages are printed.
var enabled atomic.Bool

// mu protects the capture buffer.
var mu sync.Mutex

// capture is non-nil when logs are being captured into a buffer
// (instead of written to stderr). Used by the daemon to collect
// logs per-request and send them back to the client.
var capture *bytes.Buffer

// GetEnabled returns the current verbose mode state.
func GetEnabled() bool {
	return enabled.Load()
}

// SetEnabled sets the verbose mode state.
func SetEnabled(v bool) {
	enabled.Store(v)
}

// StartCapture begins capturing log output into a buffer.
// Call StopCapture to retrieve the captured logs.
func StartCapture() {
	mu.Lock()
	capture = &bytes.Buffer{}
	mu.Unlock()
}

// StopCapture stops capturing and returns the captured log output.
func StopCapture() string {
	mu.Lock()
	defer mu.Unlock()
	if capture == nil {
		return ""
	}
	s := capture.String()
	capture = nil
	return s
}

// Printf prints a timestamped log line to stderr (or the capture buffer)
// when verbose mode is enabled.
func Printf(format string, args ...any) {
	if !enabled.Load() {
		return
	}
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("[%s] %s\n", time.Now().Format("15:04:05.000"), msg)

	mu.Lock()
	if capture != nil {
		capture.WriteString(line)
	} else {
		fmt.Fprint(os.Stderr, line)
	}
	mu.Unlock()
}
