package vlog

import (
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

// enabled controls whether verbose log messages are printed.
var enabled atomic.Bool

// GetEnabled returns the current verbose mode state.
func GetEnabled() bool {
	return enabled.Load()
}

// SetEnabled sets the verbose mode state.
func SetEnabled(v bool) {
	enabled.Store(v)
}

// Printf prints a timestamped log line to stderr when verbose mode is enabled.
func Printf(format string, args ...any) {
	if !enabled.Load() {
		return
	}
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "[%s] %s\n", time.Now().Format("15:04:05.000"), msg)
}
