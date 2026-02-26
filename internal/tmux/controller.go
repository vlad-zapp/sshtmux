package tmux

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

// CommandResult holds the result of a tmux command sent via control mode.
type CommandResult struct {
	Data  string // response data lines joined with newlines
	Error string // non-empty if %error was received
}

// Controller interface for tmux control mode interaction.
type Controller interface {
	// SendCommand sends a tmux command and waits for %begin/%end or %error response.
	SendCommand(ctx context.Context, cmd string) (*CommandResult, error)
	// SendCommandPipeline sends multiple commands in a single write and returns
	// responses in order. This eliminates SSH round-trip gaps between commands.
	SendCommandPipeline(ctx context.Context, cmds []string) ([]*CommandResult, error)
	// OutputCh returns a read-only channel that receives %output pane data.
	OutputCh() <-chan string
	// PaneID returns the ID of the session's pane (e.g., "%0").
	PaneID() string
	// SetPaneID sets the pane ID (discovered from initial output).
	SetPaneID(id string)
	// Alive returns true if the controller's read loop is still running.
	Alive() bool
	// Detach sends a detach command to exit control mode cleanly.
	Detach() error
	// Close closes the controller and stops the reader goroutine.
	Close() error
}

// RealController implements Controller using actual io.Reader/Writer streams.
// It uses a FIFO queue for command-response correlation because tmux assigns
// its own command numbers that we cannot predict (they persist across sessions).
type RealController struct {
	w      io.Writer
	mu     sync.Mutex // serializes writes and queue pushes
	paneID string
	paneIDMu sync.RWMutex

	// FIFO queue of pending command response channels.
	// Commands are serialized (mu), tmux responds in order, so FIFO works.
	queue   []chan commandResponse
	queueMu sync.Mutex

	// outputCh receives parsed %output pane data from the readLoop.
	outputCh chan string

	// startup is closed after the initial %begin/%end pair from tmux is consumed.
	startup     chan struct{}
	startupOnce sync.Once

	done   chan struct{}
	closed sync.Once
	err    error // reader goroutine error

	// Optional diagnostic logger set via SetLogFunc.
	logFunc func(format string, args ...any)
}

type commandResponse struct {
	result *CommandResult
	err    error
}

// NewController creates a new tmux control mode controller.
// The reader goroutine starts immediately to process incoming messages.
// The caller must call Close() when done.
func NewController(r io.Reader, w io.Writer) *RealController {
	c := &RealController{
		w:        w,
		outputCh: make(chan string, 65536),
		startup:  make(chan struct{}),
		done:     make(chan struct{}),
	}
	go c.readLoop(r)
	return c
}

// SetLogFunc sets an optional diagnostic logger for the controller's read loop.
// Must be called before WaitStartup for complete coverage.
func (c *RealController) SetLogFunc(f func(format string, args ...any)) {
	c.logFunc = f
}

func (c *RealController) log(format string, args ...any) {
	if f := c.logFunc; f != nil {
		f(format, args...)
	}
}

// OutputCh returns a read-only channel that receives %output pane data.
func (c *RealController) OutputCh() <-chan string {
	return c.outputCh
}

// WaitStartup blocks until the initial tmux %begin/%end handshake is consumed.
// Call this after NewController and before sending commands.
func (c *RealController) WaitStartup(ctx context.Context) error {
	select {
	case <-c.startup:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return fmt.Errorf("controller closed during startup")
	}
}

func (c *RealController) readLoop(r io.Reader) {
	defer close(c.done)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB line buffer

	var currentData []string
	inResponse := false

	for scanner.Scan() {
		line := scanner.Text()
		msg, ok := ParseLine(line)
		if !ok {
			// Data line (between %begin and %end)
			if inResponse {
				currentData = append(currentData, line)
			} else if strings.HasPrefix(line, "%") {
				c.log("readloop: unknown notification: %q", line)
			}
			continue
		}

		switch msg.Type {
		case MsgBegin:
			c.log("readloop: %%begin cmd=%d", msg.CmdNum)
			currentData = nil
			inResponse = true

		case MsgEnd:
			inResponse = false
			data := strings.Join(currentData, "\n")
			c.log("readloop: %%end cmd=%d data_len=%d", msg.CmdNum, len(data))
			c.deliverResponse(&CommandResult{Data: data})

		case MsgError:
			inResponse = false
			errMsg := strings.Join(currentData, "\n")
			c.log("readloop: %%error cmd=%d err=%q", msg.CmdNum, errMsg)
			c.deliverResponse(&CommandResult{Error: errMsg})

		case MsgOutput:
			c.log("readloop: %%output pane=%s data_len=%d data=%q", msg.PaneID, len(msg.Data), msg.Data)
			select {
			case c.outputCh <- msg.Data:
			default:
				c.log("readloop: output channel full, dropping data")
			}
		}
	}

	c.log("readloop: scanner exited err=%v", scanner.Err())
	c.err = scanner.Err()

	// Signal all pending commands that the reader is done.
	// Include scanner error in the message if available.
	errMsg := "controller closed"
	if c.err != nil {
		errMsg = fmt.Sprintf("controller closed: %v", c.err)
	}
	c.queueMu.Lock()
	pending := len(c.queue)
	for _, ch := range c.queue {
		ch <- commandResponse{err: fmt.Errorf("%s", errMsg)}
	}
	c.queue = nil
	c.queueMu.Unlock()
	c.log("readloop: cleaned up %d pending waiters", pending)
}

// deliverResponse sends a result to the first pending waiter, or signals startup.
func (c *RealController) deliverResponse(result *CommandResult) {
	c.queueMu.Lock()
	if len(c.queue) > 0 {
		ch := c.queue[0]
		c.queue = c.queue[1:]
		remaining := len(c.queue)
		c.queueMu.Unlock()
		c.log("readloop: delivered to waiter (remaining=%d)", remaining)
		ch <- commandResponse{result: result}
		return
	}
	c.queueMu.Unlock()

	// No pending command — this is the initial startup response
	c.log("readloop: no waiter in queue, signaling startup")
	c.startupOnce.Do(func() {
		close(c.startup)
	})
}

// SendCommand sends a tmux command and waits for the response.
func (c *RealController) SendCommand(ctx context.Context, cmd string) (*CommandResult, error) {
	ch := make(chan commandResponse, 1)

	c.mu.Lock()
	// Push to queue while holding mu, so send order == queue order
	c.queueMu.Lock()
	c.queue = append(c.queue, ch)
	qLen := len(c.queue)
	c.queueMu.Unlock()

	c.log("SendCommand: writing %q queue_len=%d", cmd, qLen)
	_, err := fmt.Fprintf(c.w, "%s\n", cmd)
	c.mu.Unlock()

	if err != nil {
		// Remove from queue
		c.queueMu.Lock()
		for i, qch := range c.queue {
			if qch == ch {
				c.queue = append(c.queue[:i], c.queue[i+1:]...)
				break
			}
		}
		c.queueMu.Unlock()
		c.log("SendCommand: write error: %v", err)
		return nil, fmt.Errorf("write command: %w", err)
	}

	select {
	case <-ctx.Done():
		// Remove from queue on cancellation
		c.queueMu.Lock()
		found := false
		for i, qch := range c.queue {
			if qch == ch {
				c.queue = append(c.queue[:i], c.queue[i+1:]...)
				found = true
				break
			}
		}
		qLen := len(c.queue)
		c.queueMu.Unlock()
		c.log("SendCommand: context cancelled for %q (found_in_queue=%v queue_len=%d)", cmd, found, qLen)
		return nil, ctx.Err()
	case resp := <-ch:
		return resp.result, resp.err
	case <-c.done:
		return nil, fmt.Errorf("controller closed")
	}
}

// SendCommandPipeline sends multiple commands in a single write and returns
// responses in order. All commands are written atomically (single Write call),
// so tmux processes them in sequence with no SSH round-trip gap between them.
func (c *RealController) SendCommandPipeline(ctx context.Context, cmds []string) ([]*CommandResult, error) {
	if len(cmds) == 0 {
		return nil, nil
	}

	channels := make([]chan commandResponse, len(cmds))
	for i := range channels {
		channels[i] = make(chan commandResponse, 1)
	}

	c.mu.Lock()
	c.queueMu.Lock()
	for _, ch := range channels {
		c.queue = append(c.queue, ch)
	}
	c.queueMu.Unlock()

	// Write all commands in a single call so they arrive atomically.
	var buf strings.Builder
	for _, cmd := range cmds {
		buf.WriteString(cmd)
		buf.WriteByte('\n')
	}
	_, err := io.WriteString(c.w, buf.String())
	c.mu.Unlock()

	if err != nil {
		// Remove all our channels from the queue.
		c.queueMu.Lock()
		chSet := make(map[chan commandResponse]struct{}, len(channels))
		for _, ch := range channels {
			chSet[ch] = struct{}{}
		}
		filtered := c.queue[:0]
		for _, qch := range c.queue {
			if _, ok := chSet[qch]; !ok {
				filtered = append(filtered, qch)
			}
		}
		c.queue = filtered
		c.queueMu.Unlock()
		return nil, fmt.Errorf("write pipeline: %w", err)
	}

	// Collect responses in order.
	results := make([]*CommandResult, len(cmds))
	for i, ch := range channels {
		select {
		case <-ctx.Done():
			// Clean up remaining channels from queue.
			c.queueMu.Lock()
			remaining := make(map[chan commandResponse]struct{}, len(channels)-i)
			for _, rch := range channels[i:] {
				remaining[rch] = struct{}{}
			}
			filtered := c.queue[:0]
			for _, qch := range c.queue {
				if _, ok := remaining[qch]; !ok {
					filtered = append(filtered, qch)
				}
			}
			c.queue = filtered
			c.queueMu.Unlock()
			return nil, ctx.Err()
		case resp := <-ch:
			if resp.err != nil {
				// Clean up remaining channels from queue.
				c.queueMu.Lock()
				remaining := make(map[chan commandResponse]struct{}, len(channels)-i-1)
				for _, rch := range channels[i+1:] {
					remaining[rch] = struct{}{}
				}
				filtered := c.queue[:0]
				for _, qch := range c.queue {
					if _, ok := remaining[qch]; !ok {
						filtered = append(filtered, qch)
					}
				}
				c.queue = filtered
				c.queueMu.Unlock()
				return nil, resp.err
			}
			results[i] = resp.result
		case <-c.done:
			// Clean up remaining channels from queue.
			c.queueMu.Lock()
			remaining := make(map[chan commandResponse]struct{}, len(channels)-i)
			for _, rch := range channels[i:] {
				remaining[rch] = struct{}{}
			}
			filtered := c.queue[:0]
			for _, qch := range c.queue {
				if _, ok := remaining[qch]; !ok {
					filtered = append(filtered, qch)
				}
			}
			c.queue = filtered
			c.queueMu.Unlock()
			return nil, fmt.Errorf("controller closed")
		}
	}

	return results, nil
}

// PaneID returns the current pane ID.
func (c *RealController) PaneID() string {
	c.paneIDMu.RLock()
	defer c.paneIDMu.RUnlock()
	return c.paneID
}

// SetPaneID sets the pane ID.
func (c *RealController) SetPaneID(id string) {
	c.paneIDMu.Lock()
	defer c.paneIDMu.Unlock()
	c.paneID = id
}

// Alive returns true if the readLoop goroutine is still running.
func (c *RealController) Alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// Detach sends a detach-client command.
func (c *RealController) Detach() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := fmt.Fprintf(c.w, "detach-client\n")
	return err
}

// Close shuts down the controller.
// It waits for the readLoop goroutine to finish so callers know all
// resources are released and outputCh is closed.
func (c *RealController) Close() error {
	c.closed.Do(func() {
		// The readLoop will exit when the reader is closed by the caller.
	})
	<-c.done
	return nil
}
