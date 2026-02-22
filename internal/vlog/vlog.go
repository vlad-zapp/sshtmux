package vlog

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"
)

// enabled controls whether verbose log messages are printed.
var enabled atomic.Bool

type logKey struct{}

// WithWriter returns a context that carries a per-request log writer.
func WithWriter(ctx context.Context, w io.Writer) context.Context {
	return context.WithValue(ctx, logKey{}, w)
}

// Logf writes a timestamped log line to the context's writer if present,
// otherwise falls back to global Printf.
func Logf(ctx context.Context, format string, args ...any) {
	if w, ok := ctx.Value(logKey{}).(io.Writer); ok && w != nil {
		msg := fmt.Sprintf(format, args...)
		line := fmt.Sprintf("[%s] %s\n", time.Now().Format("15:04:05.000"), msg)
		w.Write([]byte(line))
		return
	}
	Printf(format, args...)
}

// HasWriter returns true if the context carries a per-request log writer.
func HasWriter(ctx context.Context) bool {
	w, ok := ctx.Value(logKey{}).(io.Writer)
	return ok && w != nil
}

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
	line := fmt.Sprintf("[%s] %s\n", time.Now().Format("15:04:05.000"), msg)
	fmt.Fprint(os.Stderr, line)
}
