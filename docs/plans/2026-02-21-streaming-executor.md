# Streaming Executor Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace polling + capture-pane executor with %output streaming and two-phase send-keys.

**Architecture:** The executor types the command (no Enter), consumes the terminal echo via %output, presses Enter, collects real output from %output while polling @sshtmux-rv for completion. The controller routes %output notifications to a buffered channel instead of discarding them.

**Tech Stack:** Go, tmux control mode

---

### Task 1: Add outputCh to Controller

Route %output notifications to a buffered channel instead of discarding.

**Files:**
- Modify: `internal/tmux/controller.go`
- Test: `internal/tmux/controller_test.go`

**Step 1: Write failing test — %output routed to channel**

Add to `controller_test.go`:

```go
func TestControllerOutputChannel(t *testing.T) {
	_, cmdW := io.Pipe()
	respR, respW := io.Pipe()

	go func() {
		fmt.Fprintf(respW, "%%output %%0 hello\\012\n")
		fmt.Fprintf(respW, "%%output %%0 world\\012\n")
		respW.Close()
	}()

	ctrl := NewController(respR, cmdW)
	defer cmdW.Close()

	ch := ctrl.OutputCh()

	// Collect output before readLoop exits
	var got []string
	timeout := time.After(2 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case data := <-ch:
			got = append(got, data)
		case <-timeout:
			t.Fatal("timeout waiting for output")
		}
	}

	<-ctrl.done

	if len(got) != 2 {
		t.Fatalf("got %d outputs, want 2", len(got))
	}
	if got[0] != "hello\n" {
		t.Errorf("got[0] = %q, want %q", got[0], "hello\n")
	}
	if got[1] != "world\n" {
		t.Errorf("got[1] = %q, want %q", got[1], "world\n")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `make test`
Expected: FAIL — `OutputCh` method doesn't exist.

**Step 3: Implement outputCh on RealController**

In `controller.go`:

1. Add field to `RealController`:
```go
outputCh chan string // %output notifications routed here
```

2. Initialize in `NewController`:
```go
outputCh: make(chan string, 1024),
```

3. Add method:
```go
// OutputCh returns the channel receiving %output notification data.
func (c *RealController) OutputCh() <-chan string {
	return c.outputCh
}
```

4. In `readLoop`, replace `outputCount++` with non-blocking send:
```go
case MsgOutput:
	select {
	case c.outputCh <- msg.Data:
	default:
		c.log("readloop: output channel full, dropping data")
	}
```

5. Remove the `outputCount` variable and the "since last command" log.

6. Add `OutputCh() <-chan string` to the `Controller` interface.

**Step 4: Run tests**

Run: `make test`
Expected: PASS

**Step 5: Write test — high volume doesn't block readLoop**

Update `TestControllerHighVolumeOutput`:

```go
func TestControllerHighVolumeOutput(t *testing.T) {
	_, cmdW := io.Pipe()
	respR, respW := io.Pipe()

	go func() {
		for i := range 5000 {
			fmt.Fprintf(respW, "%%output %%0 line-%d\\012\n", i)
		}
		respW.Close()
	}()

	ctrl := NewController(respR, cmdW)
	defer cmdW.Close()

	// readLoop should not deadlock even if we don't drain the channel.
	// Channel is buffered (1024), excess is dropped.
	<-ctrl.done
}
```

**Step 6: Run tests**

Run: `make test-race`
Expected: PASS

**Step 7: Commit**

```
feat(tmux): route %output notifications to buffered channel
```

---

### Task 2: Add OutputCh to mock controllers

Update all mock/noop controllers to satisfy the new Controller interface.

**Files:**
- Modify: `internal/session/executor_test.go` (mockController)
- Modify: `internal/daemon/connpool_test.go` (noopController)
- Modify: `internal/client/client_test.go` (noopController)

**Step 1: Add OutputCh to mockController**

In `executor_test.go`, add field and method to `mockController`:

```go
// In struct:
outputCh chan string

// In newMockController:
outputCh: make(chan string, 1024),

// Method:
func (m *mockController) OutputCh() <-chan string {
	return m.outputCh
}
```

**Step 2: Add OutputCh to both noopControllers**

In `connpool_test.go` and `client_test.go`:

```go
// In struct:
outputCh chan string
```

Initialize in each factory/creation site (add `outputCh: make(chan string, 1024)` to struct literal).

```go
func (c *noopController) OutputCh() <-chan string {
	return c.outputCh
}
```

**Step 3: Run tests**

Run: `make test`
Expected: PASS

**Step 4: Commit**

```
refactor: add OutputCh to mock and noop controllers
```

---

### Task 3: Add FormatSendKeysLiteral helper

A new helper for `send-keys -l` (literal, no Enter).

**Files:**
- Modify: `internal/tmux/parser.go`
- Test: `internal/tmux/parser_test.go`

**Step 1: Write failing test**

In `parser_test.go`:

```go
func TestFormatSendKeysLiteral(t *testing.T) {
	got := FormatSendKeysLiteral("%0", "echo hello")
	want := "send-keys -l -t %0 'echo hello'"
	if got != want {
		t.Errorf("FormatSendKeysLiteral = %q, want %q", got, want)
	}
}

func TestFormatSendKeysEnter(t *testing.T) {
	got := FormatSendKeysEnter("%0")
	want := "send-keys -t %0 Enter"
	if got != want {
		t.Errorf("FormatSendKeysEnter = %q, want %q", got, want)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `make test`
Expected: FAIL — functions don't exist.

**Step 3: Implement**

In `parser.go`:

```go
// FormatSendKeysLiteral formats a send-keys command with -l (literal, no Enter).
func FormatSendKeysLiteral(target, text string) string {
	return fmt.Sprintf("send-keys -l -t %s %s", target, ShellQuote(text))
}

// FormatSendKeysEnter formats a send-keys command that presses Enter.
func FormatSendKeysEnter(target string) string {
	return fmt.Sprintf("send-keys -t %s Enter", target)
}
```

**Step 4: Run tests**

Run: `make test`
Expected: PASS

**Step 5: Commit**

```
feat(tmux): add FormatSendKeysLiteral and FormatSendKeysEnter helpers
```

---

### Task 4: Rewrite Executor.Exec with streaming

Replace the polling + capture-pane flow with two-phase send + %output streaming.

**Files:**
- Modify: `internal/session/executor.go`
- Test: `internal/session/executor_test.go`

**Step 1: Write failing tests for the new flow**

Replace the existing `TestExecBasic` and add new tests. The mockController's `outputCh` is used to simulate %output.

```go
// echoTail is the suffix we match in the %output echo to know typing is done.
const testEchoTail = "tmux set-option -p @sshtmux-rv $?"

func TestExecStreaming(t *testing.T) {
	mc := newMockController("%0")
	// Simulate: display-message for rv returns "0" (the exit code)
	mc.responseFunc["display-message"] = func(cmd string) string {
		if strings.Contains(cmd, "@sshtmux-rv") {
			return "0"
		}
		return ""
	}

	exec := NewExecutor(mc, "")

	// Simulate the terminal in a goroutine:
	// 1. Echo the typed command (arrives via outputCh)
	// 2. After Enter, send actual output
	go func() {
		// Wait briefly for send-keys -l to be sent
		time.Sleep(10 * time.Millisecond)
		// Echo: the terminal echoes back what we typed
		mc.outputCh <- "ls -la; tmux set-option -p @sshtmux-rv $?"
		// Wait for Enter to be pressed
		time.Sleep(10 * time.Millisecond)
		// Real output
		mc.outputCh <- "file1.txt\n"
		mc.outputCh <- "file2.txt\n"
	}()

	ctx := context.Background()
	result, err := exec.Exec(ctx, "ls -la", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if !strings.Contains(result.Output, "file1.txt") {
		t.Errorf("Output = %q, want to contain file1.txt", result.Output)
	}
}

func TestExecStreamingNoOutput(t *testing.T) {
	mc := newMockController("%0")
	mc.responseFunc["display-message"] = func(cmd string) string {
		if strings.Contains(cmd, "@sshtmux-rv") {
			return "0"
		}
		return ""
	}

	exec := NewExecutor(mc, "")

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "true; tmux set-option -p @sshtmux-rv $?"
	}()

	result, err := exec.Exec(context.Background(), "true", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Output != "" {
		t.Errorf("Output = %q, want empty", result.Output)
	}
}

func TestExecStreamingEchoInChunks(t *testing.T) {
	mc := newMockController("%0")
	mc.responseFunc["display-message"] = func(cmd string) string {
		if strings.Contains(cmd, "@sshtmux-rv") {
			return "0"
		}
		return ""
	}

	exec := NewExecutor(mc, "")

	// Echo arrives in multiple chunks
	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "echo hello; "
		mc.outputCh <- "tmux set-option"
		mc.outputCh <- " -p @sshtmux-rv $?"
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "hello\n"
	}()

	result, err := exec.Exec(context.Background(), "echo hello", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(result.Output, "hello") {
		t.Errorf("Output = %q, want to contain hello", result.Output)
	}
}

func TestExecStreamingTimeout(t *testing.T) {
	mc := newMockController("%0")
	// rv never returns a value — command never finishes
	mc.responseFunc["display-message"] = func(cmd string) string {
		return ""
	}

	exec := NewExecutor(mc, "")

	// Send echo so we get past echo phase
	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "sleep 100; tmux set-option -p @sshtmux-rv $?"
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_, err := exec.Exec(ctx, "sleep 100", 0)
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestExecStreamingEchoTimeout(t *testing.T) {
	mc := newMockController("%0")
	exec := NewExecutor(mc, "")

	// No output at all — echo never arrives
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := exec.Exec(ctx, "cmd", 0)
	if err == nil {
		t.Error("expected timeout error when echo never arrives")
	}
}

func TestExecStreamingDrainsStaleOutput(t *testing.T) {
	mc := newMockController("%0")
	mc.responseFunc["display-message"] = func(cmd string) string {
		if strings.Contains(cmd, "@sshtmux-rv") {
			return "0"
		}
		return ""
	}

	// Put stale data in the channel before Exec
	mc.outputCh <- "stale leftover\n"

	exec := NewExecutor(mc, "")

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "cmd; tmux set-option -p @sshtmux-rv $?"
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "fresh output\n"
	}()

	result, err := exec.Exec(context.Background(), "cmd", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Contains(result.Output, "stale") {
		t.Errorf("Output contains stale data: %q", result.Output)
	}
	if !strings.Contains(result.Output, "fresh output") {
		t.Errorf("Output missing fresh data: %q", result.Output)
	}
}
```

**Step 2: Run tests to verify they fail**

Run: `make test`
Expected: FAIL — old Exec still uses capture-pane.

**Step 3: Rewrite Executor.Exec**

Replace the entire `Exec` method and remove `pollForDone`, `postProcessOutput`. The new `Exec`:

```go
const echoTail = "tmux set-option -p @sshtmux-rv $?"

func (e *Executor) Exec(ctx context.Context, command string, timeout time.Duration) (*ExecResult, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	paneID := e.ctrl.PaneID()
	if paneID == "" {
		return nil, fmt.Errorf("pane ID not set")
	}

	outputCh := e.ctrl.OutputCh()
	tag := fmt.Sprintf("exec_%d", e.counter.Add(1))
	vlog.Logf(ctx, "exec: running command=%q tag=%s pane=%s", command, tag, paneID)

	// Step 1: Reset rv.
	vlog.Logf(ctx, "exec: step 1: reset rv")
	resetCmd := fmt.Sprintf("set-option -p -t %s @sshtmux-rv ''", paneID)
	if _, err := e.ctrl.SendCommand(ctx, resetCmd); err != nil {
		return nil, fmt.Errorf("reset rv: %w", err)
	}

	// Drain stale %output.
	drainChannel(outputCh)

	// Step 2: Type command (no Enter).
	tmuxBin := e.tmuxCmd()
	shellLine := fmt.Sprintf("%s; %s set-option -p @sshtmux-rv $?", command, tmuxBin)
	sendLiteral := tmux.FormatSendKeysLiteral(paneID, shellLine)
	vlog.Logf(ctx, "exec: step 2: send-keys -l")
	if _, err := e.ctrl.SendCommand(ctx, sendLiteral); err != nil {
		return nil, fmt.Errorf("send-keys literal: %w", err)
	}

	// Step 3: Consume echo.
	vlog.Logf(ctx, "exec: step 3: consuming echo")
	if err := consumeEcho(ctx, outputCh, echoTail); err != nil {
		return nil, fmt.Errorf("consume echo: %w", err)
	}
	vlog.Logf(ctx, "exec: step 3: echo consumed")

	// Step 4: Press Enter.
	sendEnter := tmux.FormatSendKeysEnter(paneID)
	vlog.Logf(ctx, "exec: step 4: press Enter")
	if _, err := e.ctrl.SendCommand(ctx, sendEnter); err != nil {
		return nil, fmt.Errorf("send-keys enter: %w", err)
	}

	// Step 5: Collect output while polling rv.
	vlog.Logf(ctx, "exec: step 5: collecting output")
	displayCmd := fmt.Sprintf("display-message -p -t %s '#{@sshtmux-rv}'", paneID)
	var output strings.Builder

	for {
		// Poll rv.
		result, err := e.ctrl.SendCommand(ctx, displayCmd)
		if err != nil {
			return nil, fmt.Errorf("poll rv: %w", err)
		}
		rv := strings.TrimSpace(result.Data)
		if rv != "" {
			// Command done. Drain remaining output from channel.
			drainOutput(outputCh, &output)

			exitCode, parseErr := strconv.Atoi(rv)
			if parseErr != nil {
				return nil, fmt.Errorf("parse exit code %q: %w", rv, parseErr)
			}

			out := strings.TrimRight(output.String(), "\n")
			vlog.Logf(ctx, "exec: done exit_code=%d output_len=%d", exitCode, len(out))
			return &ExecResult{Output: out, ExitCode: exitCode}, nil
		}

		// Not done yet — drain any available output, then wait before next poll.
		drainOutput(outputCh, &output)

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("poll rv: %w", ctx.Err())
		case <-time.After(pollInterval): // 200ms
		}
	}
}

// consumeEcho reads from outputCh until the accumulated data contains the tail string.
func consumeEcho(ctx context.Context, ch <-chan string, tail string) error {
	var buf strings.Builder
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case data := <-ch:
			buf.WriteString(data)
			if strings.Contains(buf.String(), tail) {
				return nil
			}
		}
	}
}

// drainChannel discards all currently buffered data from the channel.
func drainChannel(ch <-chan string) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// drainOutput reads all currently available data from ch into the builder.
func drainOutput(ch <-chan string, buf *strings.Builder) {
	for {
		select {
		case data := <-ch:
			buf.WriteString(data)
		default:
			return
		}
	}
}
```

Remove: `pollForDone`, `postProcessOutput`.

Change `pollInterval` from 50ms to 200ms — output now arrives in real-time via %output, so the poll only determines when we notice completion.

**Step 4: Delete old tests that test removed functions**

Remove tests: `TestPostProcessOutput`, `TestPostProcessOutputNoTrailingNewline`, `TestPostProcessOutputCRLF`, `TestPostProcessOutputNoTag`, `TestPollForDoneMultiplePolls`.

Update remaining tests that check command sequences to match the new flow (reset rv, send-keys -l, send-keys Enter, display-message rv).

**Step 5: Run tests**

Run: `make test`
Expected: PASS

**Step 6: Run race detector**

Run: `make test-race`
Expected: PASS

**Step 7: Commit**

```
feat(session): rewrite Exec with %output streaming and two-phase send
```

---

### Task 5: Rewrite Executor.RunInit with streaming

Same two-phase approach but discard all output.

**Files:**
- Modify: `internal/session/executor.go`
- Test: `internal/session/executor_test.go`

**Step 1: Write failing test**

```go
func TestRunInitStreaming(t *testing.T) {
	mc := newMockController("%0")
	exec := NewExecutor(mc, "")

	go func() {
		time.Sleep(10 * time.Millisecond)
		mc.outputCh <- "sudo -i; tmux set-option -p @sshtmux-rv $?"
		time.Sleep(10 * time.Millisecond)
		// Some output from init command
		mc.outputCh <- "root prompt stuff\n"
	}()

	// rv returns "0" for init
	mc.responseFunc["display-message"] = func(cmd string) string {
		if strings.Contains(cmd, "@sshtmux-rv") {
			return "0"
		}
		return ""
	}

	if err := exec.RunInit(context.Background(), "sudo -i"); err != nil {
		t.Fatalf("RunInit: %v", err)
	}
}
```

**Step 2: Rewrite RunInit**

Same flow as Exec but discard the output and ignore exit code. Use `initTimeout` for the per-command timeout. Keep `captureInitDiagnostics` but change it to drain %output instead of capture-pane.

```go
func (e *Executor) RunInit(ctx context.Context, command string) error {
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	paneID := e.ctrl.PaneID()
	if paneID == "" {
		return fmt.Errorf("pane ID not set")
	}

	outputCh := e.ctrl.OutputCh()
	tag := fmt.Sprintf("init_%d", e.counter.Add(1))
	vlog.Logf(ctx, "init: running %q tag=%s pane=%s", command, tag, paneID)

	// Reset rv.
	if _, err := e.ctrl.SendCommand(ctx, fmt.Sprintf("set-option -p -t %s @sshtmux-rv ''", paneID)); err != nil {
		return fmt.Errorf("reset rv: %w", err)
	}

	drainChannel(outputCh)

	// Type command.
	tmuxBin := e.tmuxCmd()
	shellLine := fmt.Sprintf("%s; %s set-option -p @sshtmux-rv $?", command, tmuxBin)
	sendLiteral := tmux.FormatSendKeysLiteral(paneID, shellLine)
	if _, err := e.ctrl.SendCommand(ctx, sendLiteral); err != nil {
		return fmt.Errorf("send-keys literal: %w", err)
	}

	// Consume echo with init timeout.
	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()

	if err := consumeEcho(initCtx, outputCh, echoTail); err != nil {
		return fmt.Errorf("consume echo: %w", err)
	}

	// Press Enter.
	if _, err := e.ctrl.SendCommand(ctx, tmux.FormatSendKeysEnter(paneID)); err != nil {
		return fmt.Errorf("send-keys enter: %w", err)
	}

	// Poll rv until non-empty.
	displayCmd := fmt.Sprintf("display-message -p -t %s '#{@sshtmux-rv}'", paneID)
	for {
		result, err := e.ctrl.SendCommand(initCtx, displayCmd)
		if err != nil {
			drainOutput(outputCh, &strings.Builder{}) // drain for diagnostics
			return fmt.Errorf("poll init rv: %w", err)
		}
		if strings.TrimSpace(result.Data) != "" {
			drainChannel(outputCh) // discard init output
			vlog.Logf(ctx, "init: %q done", command)
			return nil
		}
		drainChannel(outputCh) // discard output while waiting
		select {
		case <-initCtx.Done():
			return fmt.Errorf("init timeout: %w", initCtx.Err())
		case <-time.After(pollInterval):
		}
	}
}
```

**Step 3: Run tests**

Run: `make test-race`
Expected: PASS

**Step 4: Commit**

```
feat(session): rewrite RunInit with streaming
```

---

### Task 6: Update session_test.go integration tests

Fix the session-level tests that test Exec/RunInit through the Session wrapper.

**Files:**
- Modify: `internal/session/session_test.go`

**Step 1: Update TestSessionExec**

Use the mockController's `outputCh` to simulate terminal echo + output.

**Step 2: Update TestSessionExecTimeout**

Verify timeout when echo never arrives or rv never set.

**Step 3: Run tests**

Run: `make test-race`
Expected: PASS

**Step 4: Commit**

```
test(session): update session integration tests for streaming executor
```

---

### Task 7: Update noopController for daemon/client integration tests

The noopController needs to simulate the echo + rv flow for the daemon and client tests.

**Files:**
- Modify: `internal/daemon/connpool_test.go`
- Modify: `internal/client/client_test.go`

**Step 1: Update noopController.SendCommand**

When it receives a `send-keys -l` command, push the echoed text onto `outputCh`. When polled for rv, return "0".

```go
func (c *noopController) SendCommand(ctx context.Context, cmd string) (*tmux.CommandResult, error) {
	if strings.HasPrefix(cmd, "send-keys -l") {
		// Extract the quoted text and simulate echo
		go func() {
			// Parse the shell line from the send-keys command and echo it
			c.outputCh <- extractSendKeysText(cmd)
		}()
	}
	if strings.Contains(cmd, "@sshtmux-rv") && strings.HasPrefix(cmd, "display-message") {
		return &tmux.CommandResult{Data: "0"}, nil
	}
	return &tmux.CommandResult{}, nil
}
```

Add helper `extractSendKeysText` that extracts the quoted text from the send-keys command.

**Step 2: Run full test suite**

Run: `make test-race`
Expected: PASS

**Step 3: Commit**

```
test(daemon): update noop controllers for streaming executor
```

---

### Task 8: Clean up removed code

Remove dead code and update comments.

**Files:**
- Modify: `internal/session/executor.go`
- Modify: `internal/session/session.go`

**Step 1: Remove dead code**

- Delete `postProcessOutput` function if still present
- Delete `pollForDone` function if still present
- Remove `@sshtmux-done` references from comments
- Remove `clear-history` references from comments
- Remove `capture-pane` references from comments (except in `captureInitDiagnostics`)
- Clean up any unused imports

**Step 2: Update session.go init comments**

Remove references to old wait-for / polling patterns in comments.

**Step 3: Run tests**

Run: `make test-race`
Expected: PASS

**Step 4: Commit**

```
refactor: remove dead code from polling-based executor
```

---

### Task 9: Build and update release binaries

**Step 1: Build**

```bash
make build
```

Cross-compile:
```bash
docker run --rm -v $(pwd):/app -v sshtmux-gomod:/go/pkg/mod -w /app -e GOOS=linux -e GOARCH=amd64 golang:1.24 go build -buildvcs=false -ldflags "-s -w" -o sshtmux-linux-amd64 ./cmd/sshtmux/
docker run --rm -v $(pwd):/app -v sshtmux-gomod:/go/pkg/mod -w /app -e GOOS=windows -e GOARCH=amd64 golang:1.24 go build -buildvcs=false -ldflags "-s -w" -o sshtmux-windows-amd64.exe ./cmd/sshtmux/
```

**Step 2: Install**

```bash
make install
```

**Step 3: Commit all changes**

Final commit with the updated executor + release binaries.
