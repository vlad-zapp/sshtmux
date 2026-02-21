package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vlad-zapp/sshtmux/internal/session"
	"github.com/vlad-zapp/sshtmux/internal/tmux"
)

func mockFactory(ctrl tmux.Controller) SessionFactory {
	return func(ctx context.Context, host, user string) (*session.Session, error) {
		return session.NewFromController(ctrl, host, user), nil
	}
}

type noopController struct {
	paneID string
}

func (c *noopController) SendCommand(ctx context.Context, cmd string) (*tmux.CommandResult, error) {
	return &tmux.CommandResult{}, nil
}
func (c *noopController) OutputChan() <-chan tmux.Notification {
	return make(chan tmux.Notification)
}
func (c *noopController) PaneID() string       { return c.paneID }
func (c *noopController) SetPaneID(id string)   { c.paneID = id }
func (c *noopController) Detach() error         { return nil }
func (c *noopController) Close() error          { return nil }

func TestConnPoolGetAndReuse(t *testing.T) {
	var createCount atomic.Int32
	factory := func(ctx context.Context, host, user string) (*session.Session, error) {
		createCount.Add(1)
		ctrl := &noopController{paneID: "%0"}
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
		ctrl := &noopController{paneID: "%0"}
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
		ctrl := &noopController{paneID: "%0"}
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
		ctrl := &noopController{paneID: "%0"}
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
		ctrl := &noopController{paneID: "%0"}
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
		ctrl := &noopController{paneID: "%0"}
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
		ctrl := &noopController{paneID: "%0"}
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
		ctrl := &noopController{paneID: "%0"}
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

func TestSessionKey(t *testing.T) {
	if k := sessionKey("host1", "user1"); k != "user1@host1" {
		t.Errorf("key = %q, want %q", k, "user1@host1")
	}
	if k := sessionKey("host1", ""); k != "host1" {
		t.Errorf("key = %q, want %q", k, "host1")
	}
}
