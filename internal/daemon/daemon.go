package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/vlad-zapp/sshtmux/internal/vlog"
)

const (
	// shutdownGracePeriod is the delay before actually stopping the daemon
	// after a shutdown request. This allows the response to be sent back
	// to the client before the listener is closed.
	shutdownGracePeriod = 100 * time.Millisecond
)

// Daemon listens on a Unix socket and dispatches requests to the connection pool.
type Daemon struct {
	pool           *ConnPool
	listener       net.Listener
	commandTimeout time.Duration
	wg             sync.WaitGroup
	done           chan struct{}
}

// NewDaemon creates a new daemon with the given pool, socket path, and command timeout.
func NewDaemon(pool *ConnPool, socketPath string, commandTimeout time.Duration) (*Daemon, error) {
	// Remove stale socket file
	os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", socketPath, err)
	}

	return &Daemon{
		pool:           pool,
		listener:       listener,
		commandTimeout: commandTimeout,
		done:           make(chan struct{}),
	}, nil
}

// Serve starts accepting connections. Blocks until Stop is called.
func (d *Daemon) Serve() error {
	// Track that Serve is running so Stop waits for it
	d.wg.Add(1)
	defer d.wg.Done()

	for {
		conn, err := d.listener.Accept()
		if err != nil {
			select {
			case <-d.done:
				return nil
			default:
				log.Printf("accept error: %v", err)
				continue
			}
		}

		// Check if we're shutting down before starting new work
		select {
		case <-d.done:
			conn.Close()
			return nil
		default:
		}

		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.handleConn(conn)
		}()
	}
}

// Stop stops the daemon.
func (d *Daemon) Stop() error {
	close(d.done)
	d.listener.Close()
	d.wg.Wait()
	return d.pool.Close()
}

// SocketPath returns the listener address.
func (d *Daemon) SocketPath() string {
	return d.listener.Addr().String()
}

func (d *Daemon) handleConn(conn net.Conn) {
	defer conn.Close()

	var req Request
	if err := ReadMessage(conn, &req); err != nil {
		log.Printf("read request: %v", err)
		return
	}

	resp := d.dispatch(req)
	if err := WriteMessage(conn, &resp); err != nil {
		log.Printf("write response: %v", err)
	}
}

func (d *Daemon) dispatch(req Request) Response {
	vlog.SetEnabled(req.Verbose)
	if req.Verbose {
		vlog.StartCapture()
	}

	vlog.Printf("daemon: dispatch type=%s host=%q user=%q command=%q", req.Type, req.Host, req.User, req.Command)

	var resp Response
	switch req.Type {
	case "exec":
		resp = d.handleExec(req)
	case "disconnect":
		resp = d.handleDisconnect(req)
	case "status":
		resp = d.handleStatus()
	case "shutdown":
		go func() {
			time.Sleep(shutdownGracePeriod)
			d.Stop()
		}()
		resp = Response{Success: true, Output: "shutting down"}
	default:
		resp = Response{Error: fmt.Sprintf("unknown request type: %s", req.Type)}
	}

	if req.Verbose {
		resp.Logs = vlog.StopCapture()
	}
	return resp
}

func (d *Daemon) handleExec(req Request) Response {
	ctx, cancel := context.WithTimeout(context.Background(), d.commandTimeout)
	defer cancel()

	vlog.Printf("daemon: getting session for %s@%s", req.User, req.Host)
	sess, err := d.pool.Get(ctx, req.Host, req.User)
	if err != nil {
		return Response{Error: fmt.Sprintf("get session: %v", err)}
	}

	vlog.Printf("daemon: session ready, executing command")
	result, err := sess.Exec(ctx, req.Command, 0)
	if err != nil {
		return Response{Error: fmt.Sprintf("exec: %v", err)}
	}

	return Response{
		Success:  result.ExitCode == 0,
		Output:   result.Output,
		ExitCode: result.ExitCode,
	}
}

func (d *Daemon) handleDisconnect(req Request) Response {
	if err := d.pool.Disconnect(req.Host, req.User); err != nil {
		return Response{Error: fmt.Sprintf("disconnect: %v", err)}
	}
	return Response{Success: true, Output: "disconnected"}
}

func (d *Daemon) handleStatus() Response {
	statuses := d.pool.Status()
	data, err := json.Marshal(statuses)
	if err != nil {
		return Response{Error: fmt.Sprintf("marshal status: %v", err)}
	}
	return Response{Success: true, Output: string(data)}
}
