package vlog

import (
	"fmt"
	"os"
	"time"
)

// Enabled controls whether verbose log messages are printed.
var Enabled bool

// Printf prints a timestamped log line to stderr when verbose mode is enabled.
func Printf(format string, args ...any) {
	if !Enabled {
		return
	}
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "[%s] %s\n", time.Now().Format("15:04:05.000"), msg)
}
