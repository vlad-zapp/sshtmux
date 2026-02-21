package tmux

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

const (
	// outputChanBuffer is the buffer size for the output notification channel.
	outputChanBuffer = 4096
)

// CommandResult holds the result of a tmux command sent via control mode.
type CommandResult struct {
	Data  string // response data lines joined with newlines
	Error string // non-empty if %error was received
}

// Notification represents an asynchronous tmux notification (e.g., %output).
type Notification struct {
	Type   MessageType
	PaneID string
	Data   string
}

// Controller interface for tmux control mode interaction.
type Controller interface {
	// SendCommand sends a tmux command and waits for %begin/%end or %error response.
	SendCommand(ctx context.Context, cmd string) (*CommandResult, error)
	// OutputChan returns a channel that receives %output notifications.
	OutputChan() <-chan Notification
	// PaneID returns the ID of the session's pane (e.g., "%0").
	PaneID() string
	// SetPaneID sets the pane ID (discovered from initial output).
	SetPaneID(id string)
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

	outputCh chan Notification

	// FIFO queue of pending command response channels.
	// Commands are serialized (mu), tmux responds in order, so FIFO works.
	queue   []chan commandResponse
	queueMu sync.Mutex

	// startup is closed after the initial %begin/%end pair from tmux is consumed.
	startup     chan struct{}
	startupOnce sync.Once

	done   chan struct{}
	closed sync.Once
	err    error // reader goroutine error
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
		outputCh: make(chan Notification, outputChanBuffer),
		startup:  make(chan struct{}),
		done:     make(chan struct{}),
	}
	go c.readLoop(r)
	return c
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
	defer close(c.outputCh)
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
			}
			continue
		}

		switch msg.Type {
		case MsgBegin:
			currentData = nil
			inResponse = true

		case MsgEnd:
			inResponse = false
			data := strings.Join(currentData, "\n")
			c.deliverResponse(&CommandResult{Data: data})

		case MsgError:
			inResponse = false
			errMsg := strings.Join(currentData, "\n")
			c.deliverResponse(&CommandResult{Error: errMsg})

		case MsgOutput:
			// Non-blocking send: drop notification if buffer is full
			// to prevent the reader goroutine from deadlocking.
			select {
			case c.outputCh <- Notification{
				Type:   MsgOutput,
				PaneID: msg.PaneID,
				Data:   msg.Data,
			}:
			default:
			}
		}
	}

	c.err = scanner.Err()

	// Signal all pending commands that the reader is done.
	// Include scanner error in the message if available.
	errMsg := "controller closed"
	if c.err != nil {
		errMsg = fmt.Sprintf("controller closed: %v", c.err)
	}
	c.queueMu.Lock()
	for _, ch := range c.queue {
		ch <- commandResponse{err: fmt.Errorf("%s", errMsg)}
	}
	c.queue = nil
	c.queueMu.Unlock()
}

// deliverResponse sends a result to the first pending waiter, or signals startup.
func (c *RealController) deliverResponse(result *CommandResult) {
	c.queueMu.Lock()
	if len(c.queue) > 0 {
		ch := c.queue[0]
		c.queue = c.queue[1:]
		c.queueMu.Unlock()
		ch <- commandResponse{result: result}
		return
	}
	c.queueMu.Unlock()

	// No pending command — this is the initial startup response
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
	c.queueMu.Unlock()

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
		return nil, fmt.Errorf("write command: %w", err)
	}

	select {
	case <-ctx.Done():
		// Remove from queue on cancellation
		c.queueMu.Lock()
		for i, qch := range c.queue {
			if qch == ch {
				c.queue = append(c.queue[:i], c.queue[i+1:]...)
				break
			}
		}
		c.queueMu.Unlock()
		return nil, ctx.Err()
	case resp := <-ch:
		return resp.result, resp.err
	case <-c.done:
		return nil, fmt.Errorf("controller closed")
	}
}

// OutputChan returns the channel for receiving %output notifications.
func (c *RealController) OutputChan() <-chan Notification {
	return c.outputCh
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
