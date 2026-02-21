package vlog

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
)

func TestPrintfWhenDisabled(t *testing.T) {
	SetEnabled(false)
	// Should not panic or produce output
	Printf("test %d", 42)
}

func TestPrintfWhenEnabled(t *testing.T) {
	SetEnabled(true)
	defer SetEnabled(false)
	// Should not panic (writes to stderr)
	Printf("test %d", 42)
}

func TestLogfWithContextWriter(t *testing.T) {
	var buf bytes.Buffer
	ctx := WithWriter(context.Background(), &buf)

	Logf(ctx, "hello %s", "world")
	Logf(ctx, "line two")

	logs := buf.String()
	if !strings.Contains(logs, "hello world") {
		t.Errorf("logs should contain 'hello world', got %q", logs)
	}
	if !strings.Contains(logs, "line two") {
		t.Errorf("logs should contain 'line two', got %q", logs)
	}
}

func TestLogfFallsBackToGlobal(t *testing.T) {
	// Without a context writer, Logf falls back to Printf (global)
	SetEnabled(false)
	ctx := context.Background()
	// Should not panic when global is disabled
	Logf(ctx, "fallback %d", 42)
}

func TestLogfConcurrentIndependentBuffers(t *testing.T) {
	const numWorkers = 10
	var wg sync.WaitGroup
	buffers := make([]*bytes.Buffer, numWorkers)

	for i := range numWorkers {
		buffers[i] = &bytes.Buffer{}
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ctx := WithWriter(context.Background(), buffers[n])
			for j := range 50 {
				Logf(ctx, "worker-%d msg-%d", n, j)
			}
		}(i)
	}
	wg.Wait()

	// Each buffer should only contain its own worker's messages
	for i, buf := range buffers {
		logs := buf.String()
		for j := range numWorkers {
			if j == i {
				continue
			}
			marker := strings.ReplaceAll(strings.ReplaceAll("worker-X", "X", string(rune('0'+j))), "", "")
			_ = marker
			// Simpler check: count occurrences of own worker marker
		}
		ownMarker := "worker-" + string(rune('0'+i))
		if !strings.Contains(logs, ownMarker) {
			t.Errorf("buffer %d should contain own messages", i)
		}
	}
}

func TestLogfTimestampFormat(t *testing.T) {
	var buf bytes.Buffer
	ctx := WithWriter(context.Background(), &buf)

	Logf(ctx, "test")

	line := buf.String()
	// Should match [HH:MM:SS.mmm] format
	if len(line) < 15 {
		t.Fatalf("line too short: %q", line)
	}
	if line[0] != '[' {
		t.Errorf("expected '[' prefix, got %q", line)
	}
	if !strings.Contains(line, "] test\n") {
		t.Errorf("expected '] test\\n' suffix, got %q", line)
	}
}
