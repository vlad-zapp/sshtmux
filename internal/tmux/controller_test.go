package tmux

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// simulateTmux writes tmux control mode responses for each command received.
// It reads lines from r (commands sent by controller) and writes responses to w.
// Uses high command numbers like real tmux does.
func simulateTmux(r io.Reader, w io.Writer, responses []string) {
	buf := make([]byte, 4096)
	cmdNum := 500 // Start at a high number like real tmux
	respIdx := 0
	for {
		n, err := r.Read(buf)
		if err != nil {
			return
		}
		lines := strings.Count(string(buf[:n]), "\n")
		for range lines {
			if respIdx < len(responses) {
				data := responses[respIdx]
				respIdx++
				fmt.Fprintf(w, "%%begin 1234567890 %d 0\n", cmdNum)
				if data != "" {
					fmt.Fprintf(w, "%s\n", data)
				}
				fmt.Fprintf(w, "%%end 1234567890 %d 0\n", cmdNum)
				cmdNum++
			}
		}
	}
}

func TestControllerSendCommand(t *testing.T) {
	cmdR, cmdW := io.Pipe()
	respR, respW := io.Pipe()

	go simulateTmux(cmdR, respW, []string{"/home/user"})
	defer cmdR.Close()

	ctrl := NewController(respR, cmdW)
	defer func() {
		cmdW.Close()
		respR.Close()
		ctrl.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := ctrl.SendCommand(ctx, "display-message -p '#{pane_current_path}'")
	if err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	if result.Data != "/home/user" {
		t.Errorf("Data = %q, want %q", result.Data, "/home/user")
	}
	if result.Error != "" {
		t.Errorf("Error = %q, want empty", result.Error)
	}
}

func TestControllerMultipleCommands(t *testing.T) {
	cmdR, cmdW := io.Pipe()
	respR, respW := io.Pipe()

	go simulateTmux(cmdR, respW, []string{"result1", "result2", "result3"})
	defer cmdR.Close()

	ctrl := NewController(respR, cmdW)
	defer func() {
		cmdW.Close()
		respR.Close()
		ctrl.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for i, want := range []string{"result1", "result2", "result3"} {
		result, err := ctrl.SendCommand(ctx, fmt.Sprintf("cmd%d", i))
		if err != nil {
			t.Fatalf("SendCommand[%d]: %v", i, err)
		}
		if result.Data != want {
			t.Errorf("[%d] Data = %q, want %q", i, result.Data, want)
		}
	}
}

func TestControllerEmptyResponse(t *testing.T) {
	cmdR, cmdW := io.Pipe()
	respR, respW := io.Pipe()

	go simulateTmux(cmdR, respW, []string{""})
	defer cmdR.Close()

	ctrl := NewController(respR, cmdW)
	defer func() {
		cmdW.Close()
		respR.Close()
		ctrl.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := ctrl.SendCommand(ctx, "list-sessions")
	if err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	if result.Data != "" {
		t.Errorf("Data = %q, want empty", result.Data)
	}
}

func TestControllerErrorResponse(t *testing.T) {
	cmdR, cmdW := io.Pipe()
	respR, respW := io.Pipe()

	go func() {
		buf := make([]byte, 4096)
		cmdR.Read(buf)
		fmt.Fprintf(respW, "%%begin 1234567890 999 0\n")
		fmt.Fprintf(respW, "unknown command\n")
		fmt.Fprintf(respW, "%%error 1234567890 999 0\n")
	}()
	defer cmdR.Close()

	ctrl := NewController(respR, cmdW)
	defer func() {
		cmdW.Close()
		respR.Close()
		ctrl.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := ctrl.SendCommand(ctx, "bad-command")
	if err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	if result.Error != "unknown command" {
		t.Errorf("Error = %q, want %q", result.Error, "unknown command")
	}
}

func TestControllerContextCancellation(t *testing.T) {
	cmdR, cmdW := io.Pipe()
	respR, _ := io.Pipe()

	go func() {
		io.Copy(io.Discard, cmdR)
	}()

	ctrl := NewController(respR, cmdW)
	defer func() {
		cmdW.Close()
		cmdR.Close()
		respR.Close()
		ctrl.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ctrl.SendCommand(ctx, "test")
	if err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestControllerConcurrentCommands(t *testing.T) {
	cmdR, cmdW := io.Pipe()
	respR, respW := io.Pipe()

	go func() {
		scanner := make([]byte, 4096)
		cmdNum := 100
		for {
			n, err := cmdR.Read(scanner)
			if err != nil {
				return
			}
			lines := strings.Count(string(scanner[:n]), "\n")
			for range lines {
				num := cmdNum
				cmdNum++
				fmt.Fprintf(respW, "%%begin 1234567890 %d 0\n", num)
				fmt.Fprintf(respW, "result-%d\n", num)
				fmt.Fprintf(respW, "%%end 1234567890 %d 0\n", num)
			}
		}
	}()
	defer cmdR.Close()

	ctrl := NewController(respR, cmdW)
	defer func() {
		cmdW.Close()
		respR.Close()
		ctrl.Close()
	}()

	const numWorkers = 5
	var wg sync.WaitGroup
	errors := make(chan error, numWorkers)

	for i := range numWorkers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := ctrl.SendCommand(ctx, fmt.Sprintf("cmd-%d", n))
			if err != nil {
				errors <- fmt.Errorf("cmd-%d: %v", n, err)
				return
			}
			if result.Data == "" {
				errors <- fmt.Errorf("cmd-%d: empty result", n)
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Error(err)
	}
}

func TestControllerClosedReader(t *testing.T) {
	cmdR, cmdW := io.Pipe()
	respR, respW := io.Pipe()

	go func() {
		io.Copy(io.Discard, cmdR)
	}()

	ctrl := NewController(respR, cmdW)
	defer func() {
		cmdW.Close()
		cmdR.Close()
		ctrl.Close()
	}()

	respW.Close()
	<-ctrl.done

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := ctrl.SendCommand(ctx, "test")
	if err == nil {
		t.Error("expected error after reader closed")
	}
}

func TestControllerPaneID(t *testing.T) {
	ctrl := &RealController{}
	ctrl.SetPaneID("%5")
	if ctrl.PaneID() != "%5" {
		t.Errorf("PaneID = %q, want %%5", ctrl.PaneID())
	}
}

func TestControllerPaneIDConcurrent(t *testing.T) {
	ctrl := &RealController{}
	var wg sync.WaitGroup

	// Concurrent writers
	for i := range 100 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ctrl.SetPaneID(fmt.Sprintf("%%%d", n))
		}(i)
	}

	// Concurrent readers
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = ctrl.PaneID()
		}()
	}

	wg.Wait()
	// If we get here without -race detecting issues, the mutex works
	got := ctrl.PaneID()
	if got == "" {
		t.Error("PaneID should not be empty after concurrent sets")
	}
}

func TestControllerOutputChannel(t *testing.T) {
	_, cmdW := io.Pipe()
	respR, respW := io.Pipe()

	// Send two %output notifications, then close.
	go func() {
		fmt.Fprintf(respW, "%%output %%0 hello\\012\n")
		fmt.Fprintf(respW, "%%output %%0 world\\012\n")
		respW.Close()
	}()

	ctrl := NewController(respR, cmdW)
	defer cmdW.Close()

	// Read the two messages from OutputCh.
	timeout := time.After(2 * time.Second)

	select {
	case data := <-ctrl.OutputCh():
		if data != "hello\n" {
			t.Errorf("first output = %q, want %q", data, "hello\n")
		}
	case <-timeout:
		t.Fatal("timed out waiting for first output")
	}

	select {
	case data := <-ctrl.OutputCh():
		if data != "world\n" {
			t.Errorf("second output = %q, want %q", data, "world\n")
		}
	case <-timeout:
		t.Fatal("timed out waiting for second output")
	}

	// Wait for readLoop to finish.
	<-ctrl.done
}

func TestControllerHighVolumeOutput(t *testing.T) {
	_, cmdW := io.Pipe()
	respR, respW := io.Pipe()

	const totalMessages = 100000
	// Send many %output notifications. readLoop should not deadlock;
	// excess output beyond the 65536-buffer channel is dropped.
	go func() {
		for i := range totalMessages {
			fmt.Fprintf(respW, "%%output %%0 line-%d\\012\n", i)
		}
		respW.Close()
	}()

	ctrl := NewController(respR, cmdW)
	defer cmdW.Close()

	<-ctrl.done

	// Drain whatever is in the channel. We should have at most 65536 items
	// (the channel buffer size) since no reader was actively consuming.
	count := 0
	for {
		select {
		case _, ok := <-ctrl.OutputCh():
			if !ok {
				goto done
			}
			count++
		default:
			goto done
		}
	}
done:
	if count > 65536 {
		t.Errorf("got %d items from output channel, expected at most 65536", count)
	}
	if count == 0 {
		t.Error("expected at least some items in output channel")
	}
	t.Logf("received %d of %d output items (channel buffer=65536)", count, totalMessages)
}

func TestControllerMultilineData(t *testing.T) {
	cmdR, cmdW := io.Pipe()
	respR, respW := io.Pipe()

	go func() {
		buf := make([]byte, 4096)
		cmdR.Read(buf)
		fmt.Fprintf(respW, "%%begin 1234567890 42 0\n")
		fmt.Fprintf(respW, "line1\n")
		fmt.Fprintf(respW, "line2\n")
		fmt.Fprintf(respW, "line3\n")
		fmt.Fprintf(respW, "%%end 1234567890 42 0\n")
	}()
	defer cmdR.Close()

	ctrl := NewController(respR, cmdW)
	defer func() {
		cmdW.Close()
		respR.Close()
		ctrl.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := ctrl.SendCommand(ctx, "list-windows")
	if err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	if result.Data != "line1\nline2\nline3" {
		t.Errorf("Data = %q, want %q", result.Data, "line1\nline2\nline3")
	}
}

func TestControllerWaitStartup(t *testing.T) {
	_, cmdW := io.Pipe()
	respR, respW := io.Pipe()

	// Simulate tmux startup: initial %begin/%end with high cmd number
	go func() {
		fmt.Fprintf(respW, "%%begin 1234567890 280 0\n")
		fmt.Fprintf(respW, "%%end 1234567890 280 0\n")
	}()

	ctrl := NewController(respR, cmdW)
	defer func() {
		cmdW.Close()
		respR.Close()
		ctrl.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := ctrl.WaitStartup(ctx); err != nil {
		t.Fatalf("WaitStartup: %v", err)
	}
}

func TestControllerDetach(t *testing.T) {
	var written strings.Builder
	ctrl := &RealController{
		w:    &written,
		done: make(chan struct{}),
	}

	if err := ctrl.Detach(); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if !strings.Contains(written.String(), "detach-client") {
		t.Errorf("Detach should write 'detach-client', got %q", written.String())
	}
}

func TestControllerWaitStartupContextCancelled(t *testing.T) {
	_, cmdW := io.Pipe()
	respR, _ := io.Pipe()

	ctrl := NewController(respR, cmdW)
	defer func() {
		cmdW.Close()
		respR.Close()
		ctrl.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := ctrl.WaitStartup(ctx)
	if err != context.Canceled {
		t.Errorf("WaitStartup err = %v, want context.Canceled", err)
	}
}

func TestControllerWaitStartupControllerClosed(t *testing.T) {
	_, cmdW := io.Pipe()
	respR, respW := io.Pipe()

	ctrl := NewController(respR, cmdW)

	// Close the writer to make readLoop exit (which closes done)
	respW.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := ctrl.WaitStartup(ctx)
	if err == nil {
		t.Error("WaitStartup should fail when controller closes during startup")
	}
	if !strings.Contains(err.Error(), "controller closed") {
		t.Errorf("error = %q, want to contain 'controller closed'", err.Error())
	}

	cmdW.Close()
}

func TestControllerAlive(t *testing.T) {
	_, cmdW := io.Pipe()
	respR, respW := io.Pipe()

	ctrl := NewController(respR, cmdW)

	if !ctrl.Alive() {
		t.Error("Alive should return true while readLoop is running")
	}

	respW.Close()
	<-ctrl.done

	if ctrl.Alive() {
		t.Error("Alive should return false after readLoop exits")
	}

	cmdW.Close()
}

func TestControllerSendCommandWriteError(t *testing.T) {
	cmdR, cmdW := io.Pipe()
	respR, _ := io.Pipe()

	go func() {
		io.Copy(io.Discard, cmdR)
	}()

	ctrl := NewController(respR, cmdW)

	// Close the write pipe to force a write error
	cmdW.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := ctrl.SendCommand(ctx, "test")
	if err == nil {
		t.Error("expected error after writer closed")
	}

	cmdR.Close()
	respR.Close()
}

func TestControllerWaitStartupThenCommand(t *testing.T) {
	cmdR, cmdW := io.Pipe()
	respR, respW := io.Pipe()

	// Simulate tmux: startup response, then command response
	go func() {
		// Startup response
		fmt.Fprintf(respW, "%%begin 1234567890 280 0\n")
		fmt.Fprintf(respW, "%%end 1234567890 280 0\n")
		// Wait for command, then respond
		buf := make([]byte, 4096)
		cmdR.Read(buf)
		fmt.Fprintf(respW, "%%begin 1234567890 281 0\n")
		fmt.Fprintf(respW, "%%0\n")
		fmt.Fprintf(respW, "%%end 1234567890 281 0\n")
	}()
	defer cmdR.Close()

	ctrl := NewController(respR, cmdW)
	defer func() {
		cmdW.Close()
		respR.Close()
		ctrl.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := ctrl.WaitStartup(ctx); err != nil {
		t.Fatalf("WaitStartup: %v", err)
	}

	result, err := ctrl.SendCommand(ctx, "display-message -p '#{pane_id}'")
	if err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	if result.Data != "%0" {
		t.Errorf("Data = %q, want %%0", result.Data)
	}
}

func TestSendCommandPipeline(t *testing.T) {
	cmdR, cmdW := io.Pipe()
	respR, respW := io.Pipe()

	go simulateTmux(cmdR, respW, []string{"result1", "result2"})
	defer cmdR.Close()

	ctrl := NewController(respR, cmdW)
	defer func() {
		cmdW.Close()
		respR.Close()
		ctrl.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	results, err := ctrl.SendCommandPipeline(ctx, []string{"cmd1", "cmd2"})
	if err != nil {
		t.Fatalf("SendCommandPipeline: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Data != "result1" {
		t.Errorf("results[0].Data = %q, want %q", results[0].Data, "result1")
	}
	if results[1].Data != "result2" {
		t.Errorf("results[1].Data = %q, want %q", results[1].Data, "result2")
	}
}

func TestSendCommandPipelineEmpty(t *testing.T) {
	ctrl := &RealController{done: make(chan struct{})}
	results, err := ctrl.SendCommandPipeline(context.Background(), nil)
	if err != nil {
		t.Fatalf("SendCommandPipeline(nil): %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results, got %v", results)
	}
}

func TestSendCommandPipelineContextCancel(t *testing.T) {
	cmdR, cmdW := io.Pipe()
	respR, _ := io.Pipe() // no responses — will block

	go func() {
		io.Copy(io.Discard, cmdR)
	}()

	ctrl := NewController(respR, cmdW)
	defer func() {
		cmdW.Close()
		cmdR.Close()
		respR.Close()
		ctrl.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := ctrl.SendCommandPipeline(ctx, []string{"cmd1", "cmd2"})
	if err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}

	// Verify no dangling channels in the queue
	ctrl.queueMu.Lock()
	qLen := len(ctrl.queue)
	ctrl.queueMu.Unlock()
	if qLen != 0 {
		t.Errorf("queue length = %d, want 0 after cancellation", qLen)
	}
}

func TestSendCommandPipelineWriteError(t *testing.T) {
	cmdR, cmdW := io.Pipe()
	respR, _ := io.Pipe()

	go func() {
		io.Copy(io.Discard, cmdR)
	}()

	ctrl := NewController(respR, cmdW)

	// Close the write pipe to force a write error
	cmdW.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := ctrl.SendCommandPipeline(ctx, []string{"cmd1", "cmd2"})
	if err == nil {
		t.Error("expected error after writer closed")
	}

	// Verify no dangling channels in the queue
	ctrl.queueMu.Lock()
	qLen := len(ctrl.queue)
	ctrl.queueMu.Unlock()
	if qLen != 0 {
		t.Errorf("queue length = %d, want 0 after write error", qLen)
	}

	cmdR.Close()
	respR.Close()
}

func TestSendCommandPipelineThenSendCommand(t *testing.T) {
	cmdR, cmdW := io.Pipe()
	respR, respW := io.Pipe()

	go simulateTmux(cmdR, respW, []string{"p1", "p2", "single"})
	defer cmdR.Close()

	ctrl := NewController(respR, cmdW)
	defer func() {
		cmdW.Close()
		respR.Close()
		ctrl.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Pipeline first
	results, err := ctrl.SendCommandPipeline(ctx, []string{"cmd1", "cmd2"})
	if err != nil {
		t.Fatalf("SendCommandPipeline: %v", err)
	}
	if results[0].Data != "p1" || results[1].Data != "p2" {
		t.Errorf("pipeline results = %q, %q; want p1, p2", results[0].Data, results[1].Data)
	}

	// Then a regular SendCommand
	result, err := ctrl.SendCommand(ctx, "cmd3")
	if err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	if result.Data != "single" {
		t.Errorf("Data = %q, want %q", result.Data, "single")
	}
}
