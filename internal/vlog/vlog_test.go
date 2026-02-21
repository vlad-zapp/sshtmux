package vlog

import (
	"strings"
	"sync"
	"testing"
)

func TestPrintfWhenDisabled(t *testing.T) {
	SetEnabled(false)
	// Should not panic or produce output
	Printf("test %d", 42)
}

func TestStartStopCapture(t *testing.T) {
	SetEnabled(true)
	defer SetEnabled(false)

	StartCapture()
	Printf("hello %s", "world")
	Printf("line two")
	logs := StopCapture()

	if !strings.Contains(logs, "hello world") {
		t.Errorf("logs should contain 'hello world', got %q", logs)
	}
	if !strings.Contains(logs, "line two") {
		t.Errorf("logs should contain 'line two', got %q", logs)
	}
}

func TestCaptureReturnsEmptyWhenNoLogs(t *testing.T) {
	SetEnabled(false)

	StartCapture()
	Printf("should not appear")
	logs := StopCapture()

	if logs != "" {
		t.Errorf("logs should be empty when disabled, got %q", logs)
	}
}

func TestStopCaptureWithoutStart(t *testing.T) {
	// Should not panic, returns empty
	logs := StopCapture()
	if logs != "" {
		t.Errorf("expected empty, got %q", logs)
	}
}

func TestCaptureIsolation(t *testing.T) {
	SetEnabled(true)
	defer SetEnabled(false)

	// First capture
	StartCapture()
	Printf("first")
	logs1 := StopCapture()

	// Second capture should not contain first's logs
	StartCapture()
	Printf("second")
	logs2 := StopCapture()

	if !strings.Contains(logs1, "first") {
		t.Errorf("logs1 should contain 'first', got %q", logs1)
	}
	if strings.Contains(logs2, "first") {
		t.Errorf("logs2 should not contain 'first', got %q", logs2)
	}
	if !strings.Contains(logs2, "second") {
		t.Errorf("logs2 should contain 'second', got %q", logs2)
	}
}

func TestCaptureConcurrentSafe(t *testing.T) {
	SetEnabled(true)
	defer SetEnabled(false)

	// Concurrent Printf calls during capture should not race
	StartCapture()
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			Printf("goroutine %d", n)
		}(i)
	}
	wg.Wait()
	logs := StopCapture()

	// All 10 goroutines should have logged
	for i := range 10 {
		if !strings.Contains(logs, "goroutine") {
			t.Errorf("missing goroutine %d in logs: %q", i, logs)
			break
		}
	}
}
