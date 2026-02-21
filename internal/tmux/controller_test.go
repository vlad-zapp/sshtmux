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

func TestControllerOutputNotifications(t *testing.T) {
	_, cmdW := io.Pipe()
	respR, respW := io.Pipe()

	go func() {
		fmt.Fprintf(respW, "%%output %%0 hello\\012\n")
		fmt.Fprintf(respW, "%%output %%0 world\\012\n")
		respW.Close()
	}()

	ctrl := NewController(respR, cmdW)
	defer func() {
		cmdW.Close()
		ctrl.Close()
	}()

	var notifications []Notification
	timeout := time.After(2 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case n := <-ctrl.OutputChan():
			notifications = append(notifications, n)
		case <-timeout:
			t.Fatal("timeout waiting for notifications")
		}
	}

	if len(notifications) != 2 {
		t.Fatalf("got %d notifications, want 2", len(notifications))
	}
	if notifications[0].Data != "hello\n" {
		t.Errorf("[0] Data = %q, want %q", notifications[0].Data, "hello\n")
	}
	if notifications[1].Data != "world\n" {
		t.Errorf("[1] Data = %q, want %q", notifications[1].Data, "world\n")
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

func TestControllerHighVolumeOutput(t *testing.T) {
	_, cmdW := io.Pipe()
	respR, respW := io.Pipe()

	const numNotifications = 1000

	go func() {
		for i := range numNotifications {
			fmt.Fprintf(respW, "%%output %%0 line-%d\\012\n", i)
		}
		respW.Close()
	}()

	ctrl := NewController(respR, cmdW)
	defer func() {
		cmdW.Close()
	}()

	// Wait for readLoop to finish processing all input
	<-ctrl.done

	// Count all notifications that survived (non-blocking send may drop some)
	count := 0
	for range ctrl.OutputChan() {
		count++
	}
	if count == 0 {
		t.Error("received 0 notifications, expected at least some")
	}
	if count > numNotifications {
		t.Errorf("received %d notifications, but only sent %d", count, numNotifications)
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

func TestControllerOutputChannelFull(t *testing.T) {
	_, cmdW := io.Pipe()
	respR, respW := io.Pipe()

	// Send more notifications than the buffer can hold without reading
	ctrl := NewController(respR, cmdW)
	defer func() {
		cmdW.Close()
		respR.Close()
	}()

	// Fill buffer by sending outputChanBuffer+100 notifications.
	// With non-blocking send, the readLoop should not deadlock.
	go func() {
		for i := range outputChanBuffer + 100 {
			fmt.Fprintf(respW, "%%output %%0 line-%d\\012\n", i)
		}
		respW.Close()
	}()

	// Wait for readLoop to finish (it should not deadlock)
	<-ctrl.done
}

func TestControllerClosedOutputChan(t *testing.T) {
	_, cmdW := io.Pipe()
	respR, respW := io.Pipe()

	ctrl := NewController(respR, cmdW)

	// Close the reader side to make readLoop exit
	respW.Close()

	// Wait for readLoop to finish
	<-ctrl.done

	// OutputChan should be closed; reading should get zero value + !ok
	select {
	case _, ok := <-ctrl.OutputChan():
		if ok {
			t.Error("expected channel to be closed")
		}
	case <-time.After(time.Second):
		t.Error("timeout: OutputChan should be closed after readLoop exits")
	}

	cmdW.Close()
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
