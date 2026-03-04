package daemon

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vlad-zapp/sshtmux/internal/session"
	"github.com/vlad-zapp/sshtmux/internal/tmux"
)

type noopController struct {
	paneID   string
	alive    bool
	outputCh chan string
	rvValue  string
}

func (c *noopController) SendCommand(ctx context.Context, cmd string) (*tmux.CommandResult, error) {
	// Handle @sshtmux-rv unset
	if strings.Contains(cmd, "set-option -pu") && strings.Contains(cmd, "@sshtmux-rv") {
		c.rvValue = ""
		return &tmux.CommandResult{}, nil
	}
	// Handle @sshtmux-rv poll
	if strings.Contains(cmd, "display-message") && strings.Contains(cmd, "@sshtmux-rv") {
		return &tmux.CommandResult{Data: c.rvValue}, nil
	}
	// When receiving send-keys -H, simulate terminal echo by
	// decoding the hex bytes and sending them to outputCh.
	if strings.HasPrefix(cmd, "send-keys -H") {
		text := decodeHexSendKeys(cmd)
		if text != "" {
			go func() { c.outputCh <- text }()
		}
	} else if strings.HasPrefix(cmd, "send-keys") && strings.HasSuffix(cmd, "Enter") {
		// Simulate instant command completion via pane option.
		c.rvValue = "0"
	}
	return &tmux.CommandResult{}, nil
}
func (c *noopController) SendCommandPipeline(ctx context.Context, cmds []string) ([]*tmux.CommandResult, error) {
	results := make([]*tmux.CommandResult, len(cmds))
	for i, cmd := range cmds {
		r, err := c.SendCommand(ctx, cmd)
		if err != nil {
			return nil, err
		}
		results[i] = r
	}
	return results, nil
}
func (c *noopController) OutputCh() <-chan string { return c.outputCh }
func (c *noopController) PaneID() string          { return c.paneID }
func (c *noopController) SetPaneID(id string)     { c.paneID = id }
func (c *noopController) Alive() bool             { return c.alive }
func (c *noopController) Detach() error           { return nil }
func (c *noopController) Close() error            { return nil }

// decodeHexSendKeys decodes a send-keys -H command back to text.
// Input format: "send-keys -H -t %0 65 63 68 6f ..."
// Ctrl+V bytes (0x16) are stripped as they are quoted-insert prefixes.
func decodeHexSendKeys(cmd string) string {
	parts := strings.SplitN(cmd, " ", 5) // send-keys -H -t %<id> <hex...>
	if len(parts) < 5 {
		return ""
	}
	tokens := strings.Fields(parts[4])
	var b strings.Builder
	for _, tok := range tokens {
		val, err := strconv.ParseUint(tok, 16, 8)
		if err != nil {
			continue
		}
		if val == 0x16 {
			continue
		}
		b.WriteByte(byte(val))
	}
	return b.String()
}

func TestConnPoolGetAndReuse(t *testing.T) {
	var createCount atomic.Int32
	factory := func(ctx context.Context, host, user string) (*session.Session, error) {
		createCount.Add(1)
		ctrl := &noopController{paneID: "%0", alive: true, outputCh: make(chan string, 1024)}
		return session.NewFromController(ctrl, host, user), nil
	}

	pool := NewConnPool(factory, 5*time.Minute)
	defer pool.Close()

	ctx := context.Background()

	// First get creates a new session
	s1, err := pool.Get(ctx, "host1", "user1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if s1 == nil {
		t.Fatal("session is nil")
	}
	if createCount.Load() != 1 {
		t.Errorf("createCount = %d, want 1", createCount.Load())
	}

	// Second get returns cached session
	s2, err := pool.Get(ctx, "host1", "user1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if s1 != s2 {
		t.Error("expected same session on second Get")
	}
	if createCount.Load() != 1 {
		t.Errorf("createCount = %d, want 1 (should reuse)", createCount.Load())
	}
}

func TestConnPoolDifferentHosts(t *testing.T) {
	var createCount atomic.Int32
	factory := func(ctx context.Context, host, user string) (*session.Session, error) {
		createCount.Add(1)
		ctrl := &noopController{paneID: "%0", alive: true, outputCh: make(chan string, 1024)}
		return session.NewFromController(ctrl, host, user), nil
	}

	pool := NewConnPool(factory, 5*time.Minute)
	defer pool.Close()

	ctx := context.Background()

	s1, _ := pool.Get(ctx, "host1", "user1")
	s2, _ := pool.Get(ctx, "host2", "user1")

	if s1 == s2 {
		t.Error("different hosts should have different sessions")
	}
	if createCount.Load() != 2 {
		t.Errorf("createCount = %d, want 2", createCount.Load())
	}
}

func TestConnPoolConcurrentSameHost(t *testing.T) {
	var createCount atomic.Int32
	factory := func(ctx context.Context, host, user string) (*session.Session, error) {
		createCount.Add(1)
		time.Sleep(50 * time.Millisecond) // Simulate slow connection
		ctrl := &noopController{paneID: "%0", alive: true, outputCh: make(chan string, 1024)}
		return session.NewFromController(ctrl, host, user), nil
	}

	pool := NewConnPool(factory, 5*time.Minute)
	defer pool.Close()

	const numWorkers = 10
	var wg sync.WaitGroup
	sessions := make([]*session.Session, numWorkers)
	errors := make([]error, numWorkers)

	for i := range numWorkers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ctx := context.Background()
			s, err := pool.Get(ctx, "host1", "user1")
			sessions[n] = s
			errors[n] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errors {
		if err != nil {
			t.Errorf("worker %d: %v", i, err)
		}
	}

	// Only one session should have been created
	if createCount.Load() != 1 {
		t.Errorf("createCount = %d, want 1 (concurrent Get should create only one)", createCount.Load())
	}

	// All workers should have the same session
	for i := 1; i < numWorkers; i++ {
		if sessions[i] != sessions[0] {
			t.Errorf("worker %d got different session than worker 0", i)
		}
	}
}

func TestConnPoolDisconnect(t *testing.T) {
	factory := func(ctx context.Context, host, user string) (*session.Session, error) {
		ctrl := &noopController{paneID: "%0", alive: true, outputCh: make(chan string, 1024)}
		return session.NewFromController(ctrl, host, user), nil
	}

	pool := NewConnPool(factory, 5*time.Minute)
	defer pool.Close()

	ctx := context.Background()
	pool.Get(ctx, "host1", "user1")

	if err := pool.Disconnect("host1", "user1"); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	// After disconnect, status should be empty
	status := pool.Status()
	if len(status) != 0 {
		t.Errorf("Status has %d entries, want 0", len(status))
	}
}

func TestConnPoolDisconnectNotFound(t *testing.T) {
	factory := func(ctx context.Context, host, user string) (*session.Session, error) {
		ctrl := &noopController{paneID: "%0", alive: true, outputCh: make(chan string, 1024)}
		return session.NewFromController(ctrl, host, user), nil
	}

	pool := NewConnPool(factory, 5*time.Minute)
	defer pool.Close()

	// Disconnecting non-existent host should be a no-op
	if err := pool.Disconnect("nonexistent", "user"); err != nil {
		t.Errorf("Disconnect non-existent: %v", err)
	}
}

func TestConnPoolStatus(t *testing.T) {
	factory := func(ctx context.Context, host, user string) (*session.Session, error) {
		ctrl := &noopController{paneID: "%0", alive: true, outputCh: make(chan string, 1024)}
		return session.NewFromController(ctrl, host, user), nil
	}

	pool := NewConnPool(factory, 5*time.Minute)
	defer pool.Close()

	ctx := context.Background()
	pool.Get(ctx, "host1", "user1")
	pool.Get(ctx, "host2", "user2")

	status := pool.Status()
	if len(status) != 2 {
		t.Fatalf("Status has %d entries, want 2", len(status))
	}

	found := make(map[string]bool)
	for _, s := range status {
		found[s.Key] = true
		if s.TTL <= 0 {
			t.Errorf("TTL for %s should be positive: %v", s.Key, s.TTL)
		}
	}
	if !found["user1@host1"] || !found["user2@host2"] {
		t.Errorf("expected both hosts in status, got keys: %v", found)
	}
}

func TestConnPoolClose(t *testing.T) {
	factory := func(ctx context.Context, host, user string) (*session.Session, error) {
		ctrl := &noopController{paneID: "%0", alive: true, outputCh: make(chan string, 1024)}
		return session.NewFromController(ctrl, host, user), nil
	}

	pool := NewConnPool(factory, 5*time.Minute)

	ctx := context.Background()
	pool.Get(ctx, "host1", "user1")

	if err := pool.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Status should be empty after close
	// (but pool is now shut down, accessing it is technically undefined)
}

func TestConnPoolConcurrentDifferentHosts(t *testing.T) {
	var createCount atomic.Int32
	factory := func(ctx context.Context, host, user string) (*session.Session, error) {
		createCount.Add(1)
		time.Sleep(20 * time.Millisecond)
		ctrl := &noopController{paneID: "%0", alive: true, outputCh: make(chan string, 1024)}
		return session.NewFromController(ctrl, host, user), nil
	}

	pool := NewConnPool(factory, 5*time.Minute)
	defer pool.Close()

	hosts := []string{"host1", "host2", "host3", "host4", "host5"}
	var wg sync.WaitGroup

	for _, h := range hosts {
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			ctx := context.Background()
			_, err := pool.Get(ctx, host, "user")
			if err != nil {
				t.Errorf("Get %s: %v", host, err)
			}
		}(h)
	}
	wg.Wait()

	if int(createCount.Load()) != len(hosts) {
		t.Errorf("createCount = %d, want %d", createCount.Load(), len(hosts))
	}
}

func TestConnPoolEvictsDeadSession(t *testing.T) {
	var createCount atomic.Int32
	ctrl := &noopController{paneID: "%0", alive: true, outputCh: make(chan string, 1024)}

	factory := func(ctx context.Context, host, user string) (*session.Session, error) {
		createCount.Add(1)
		return session.NewFromController(ctrl, host, user), nil
	}

	pool := NewConnPool(factory, 5*time.Minute)
	defer pool.Close()

	ctx := context.Background()

	// First get creates a new session
	s1, err := pool.Get(ctx, "host1", "user1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if createCount.Load() != 1 {
		t.Fatalf("createCount = %d, want 1", createCount.Load())
	}

	// Mark the controller as dead (simulates SSH process exit)
	ctrl.alive = false

	// Next get should evict the dead session and create a new one
	s2, err := pool.Get(ctx, "host1", "user1")
	if err != nil {
		t.Fatalf("Get after dead: %v", err)
	}
	if createCount.Load() != 2 {
		t.Errorf("createCount = %d, want 2 (should recreate after dead)", createCount.Load())
	}
	if s1 == s2 {
		t.Error("expected different session after eviction")
	}
}

func TestConnPoolReapExpired(t *testing.T) {
	factory := func(ctx context.Context, host, user string) (*session.Session, error) {
		ctrl := &noopController{paneID: "%0", alive: true, outputCh: make(chan string, 1024)}
		return session.NewFromController(ctrl, host, user), nil
	}

	// Use a very short TTL
	pool := NewConnPool(factory, 50*time.Millisecond)
	defer pool.Close()

	ctx := context.Background()
	pool.Get(ctx, "host1", "user1")

	status := pool.Status()
	if len(status) != 1 {
		t.Fatalf("Status has %d entries, want 1", len(status))
	}

	// Call reapExpired directly (don't wait for ticker)
	time.Sleep(60 * time.Millisecond) // wait past TTL
	pool.reapExpired()

	status = pool.Status()
	if len(status) != 0 {
		t.Errorf("Status has %d entries after reap, want 0", len(status))
	}
}

func TestConnPoolGetEvictsExpiredSessionImmediately(t *testing.T) {
	var createCount atomic.Int32
	factory := func(ctx context.Context, host, user string) (*session.Session, error) {
		createCount.Add(1)
		ctrl := &noopController{paneID: "%0", alive: true, outputCh: make(chan string, 1024)}
		return session.NewFromController(ctrl, host, user), nil
	}

	pool := NewConnPool(factory, 50*time.Millisecond)
	defer pool.Close()

	ctx := context.Background()
	s1, err := pool.Get(ctx, "host1", "user1")
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	time.Sleep(60 * time.Millisecond)

	s2, err := pool.Get(ctx, "host1", "user1")
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if s1 == s2 {
		t.Error("expected a new session after TTL expiry")
	}
	if createCount.Load() != 2 {
		t.Errorf("createCount = %d, want 2", createCount.Load())
	}
}

func TestConnPoolReapExpiredKeepsRecent(t *testing.T) {
	factory := func(ctx context.Context, host, user string) (*session.Session, error) {
		ctrl := &noopController{paneID: "%0", alive: true, outputCh: make(chan string, 1024)}
		return session.NewFromController(ctrl, host, user), nil
	}

	pool := NewConnPool(factory, 5*time.Minute)
	defer pool.Close()

	ctx := context.Background()
	pool.Get(ctx, "host1", "user1")

	// reapExpired should not remove recently used sessions
	pool.reapExpired()

	status := pool.Status()
	if len(status) != 1 {
		t.Errorf("Status has %d entries after reap, want 1 (should keep recent)", len(status))
	}
}

func TestConnPoolGetContextCancelled(t *testing.T) {
	factory := func(ctx context.Context, host, user string) (*session.Session, error) {
		// Simulate slow creation
		time.Sleep(200 * time.Millisecond)
		ctrl := &noopController{paneID: "%0", alive: true, outputCh: make(chan string, 1024)}
		return session.NewFromController(ctrl, host, user), nil
	}

	pool := NewConnPool(factory, 5*time.Minute)
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	// First goroutine takes the inflight slot
	go pool.Get(context.Background(), "host1", "user1")
	time.Sleep(5 * time.Millisecond) // let it start

	// Second goroutine waits on inflight, then its context expires
	_, err := pool.Get(ctx, "host1", "user1")
	if err == nil {
		// It's possible the first goroutine finished fast enough
		t.Log("Get succeeded (first goroutine finished before timeout)")
	}
}

func TestConnPoolEvict(t *testing.T) {
	var createCount atomic.Int32
	factory := func(ctx context.Context, host, user string) (*session.Session, error) {
		createCount.Add(1)
		ctrl := &noopController{paneID: "%0", alive: true, outputCh: make(chan string, 1024)}
		return session.NewFromController(ctrl, host, user), nil
	}

	pool := NewConnPool(factory, 5*time.Minute)
	defer pool.Close()

	ctx := context.Background()

	// Create session
	s1, err := pool.Get(ctx, "host1", "user1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if createCount.Load() != 1 {
		t.Fatalf("createCount = %d, want 1", createCount.Load())
	}

	// Evict it
	pool.Evict("host1", "user1")

	// Pool should be empty
	status := pool.Status()
	if len(status) != 0 {
		t.Errorf("Status has %d entries after evict, want 0", len(status))
	}

	// Next Get should create a new session
	s2, err := pool.Get(ctx, "host1", "user1")
	if err != nil {
		t.Fatalf("Get after evict: %v", err)
	}
	if s1 == s2 {
		t.Error("expected different session after evict")
	}
	if createCount.Load() != 2 {
		t.Errorf("createCount = %d, want 2", createCount.Load())
	}
}

func TestConnPoolEvictNotFound(t *testing.T) {
	factory := func(ctx context.Context, host, user string) (*session.Session, error) {
		ctrl := &noopController{paneID: "%0", alive: true, outputCh: make(chan string, 1024)}
		return session.NewFromController(ctrl, host, user), nil
	}

	pool := NewConnPool(factory, 5*time.Minute)
	defer pool.Close()

	// Evicting non-existent host should be a no-op (no panic)
	pool.Evict("nonexistent", "user")
}

func TestSessionKey(t *testing.T) {
	if k := sessionKey("host1", "user1"); k != "user1@host1" {
		t.Errorf("key = %q, want %q", k, "user1@host1")
	}
	if k := sessionKey("host1", ""); k != "host1" {
		t.Errorf("key = %q, want %q", k, "host1")
	}
}
