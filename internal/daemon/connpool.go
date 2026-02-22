package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/vlad-zapp/sshtmux/internal/session"
	"github.com/vlad-zapp/sshtmux/internal/vlog"
)

// SessionFactory creates a session for a given host.
type SessionFactory func(ctx context.Context, host, user string) (*session.Session, error)

type poolEntry struct {
	sess     *session.Session
	lastUsed time.Time
}

// ConnPool manages per-host session caching with TTL-based expiry.
type ConnPool struct {
	mu       sync.Mutex
	sessions map[string]*poolEntry
	inflight map[string]chan struct{} // serializes creation per host
	factory  SessionFactory
	ttl      time.Duration

	done chan struct{}
	wg   sync.WaitGroup
}

// NewConnPool creates a new connection pool.
func NewConnPool(factory SessionFactory, ttl time.Duration) *ConnPool {
	p := &ConnPool{
		sessions: make(map[string]*poolEntry),
		inflight: make(map[string]chan struct{}),
		factory:  factory,
		ttl:      ttl,
		done:     make(chan struct{}),
	}
	p.wg.Add(1)
	go p.reaper()
	return p
}

func sessionKey(host, user string) string {
	if user == "" {
		return host
	}
	return user + "@" + host
}

// Get returns a cached session or creates a new one.
// Concurrent calls for the same host are serialized (only one session created).
func (p *ConnPool) Get(ctx context.Context, host, user string) (*session.Session, error) {
	key := sessionKey(host, user)

	for {
		p.mu.Lock()

		// Check cache
		if entry, ok := p.sessions[key]; ok {
			if entry.sess.Alive() {
				entry.lastUsed = time.Now()
				p.mu.Unlock()
				vlog.Logf(ctx, "pool: cache hit for %s", key)
				return entry.sess, nil
			}
			// Session is dead, evict from cache
			vlog.Logf(ctx, "pool: evicting dead session for %s", key)
			delete(p.sessions, key)
			go entry.sess.Close()
		}

		// Check if someone else is creating this session
		if wait, ok := p.inflight[key]; ok {
			p.mu.Unlock()
			// Wait for the other goroutine to finish
			select {
			case <-wait:
				continue // Re-check cache
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		// We'll create it - mark as inflight
		wait := make(chan struct{})
		p.inflight[key] = wait
		p.mu.Unlock()

		// Create session outside of lock
		vlog.Logf(ctx, "pool: cache miss for %s, creating new session", key)
		sess, err := p.factory(ctx, host, user)

		p.mu.Lock()
		delete(p.inflight, key)
		close(wait) // unblock waiters

		if err != nil {
			p.mu.Unlock()
			return nil, fmt.Errorf("create session: %w", err)
		}

		p.sessions[key] = &poolEntry{
			sess:     sess,
			lastUsed: time.Now(),
		}
		p.mu.Unlock()
		return sess, nil
	}
}

// Evict removes a session from the pool and closes it asynchronously.
// Used when a command times out and the session is in a dirty state.
func (p *ConnPool) Evict(host, user string) {
	key := sessionKey(host, user)
	p.mu.Lock()
	entry, ok := p.sessions[key]
	if ok {
		delete(p.sessions, key)
	}
	p.mu.Unlock()
	if ok {
		go entry.sess.Close()
	}
}

// Disconnect removes and closes a session for the given host.
func (p *ConnPool) Disconnect(host, user string) error {
	key := sessionKey(host, user)
	p.mu.Lock()
	entry, ok := p.sessions[key]
	if ok {
		delete(p.sessions, key)
	}
	p.mu.Unlock()
	if ok {
		return entry.sess.Close()
	}
	return nil
}

// Status returns a list of active connection keys and their last used times.
func (p *ConnPool) Status() []ConnectionStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	var result []ConnectionStatus
	for key, entry := range p.sessions {
		result = append(result, ConnectionStatus{
			Key:      key,
			Host:     entry.sess.Host,
			User:     entry.sess.User,
			LastUsed: entry.lastUsed,
			TTL:      p.ttl - time.Since(entry.lastUsed),
		})
	}
	return result
}

// ConnectionStatus describes a cached connection.
type ConnectionStatus struct {
	Key      string
	Host     string
	User     string
	LastUsed time.Time
	TTL      time.Duration
}

// Close stops the reaper and closes all sessions.
func (p *ConnPool) Close() error {
	close(p.done)
	p.wg.Wait()

	p.mu.Lock()
	defer p.mu.Unlock()
	var firstErr error
	for key, entry := range p.sessions {
		if err := entry.sess.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(p.sessions, key)
	}
	return firstErr
}

func (p *ConnPool) reaper() {
	defer p.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.reapExpired()
		case <-p.done:
			return
		}
	}
}

func (p *ConnPool) reapExpired() {
	p.mu.Lock()
	var expired []string
	for key, entry := range p.sessions {
		if time.Since(entry.lastUsed) > p.ttl {
			expired = append(expired, key)
		}
	}
	// Remove and collect sessions to close
	toClose := make([]*session.Session, 0, len(expired))
	for _, key := range expired {
		if entry, ok := p.sessions[key]; ok {
			toClose = append(toClose, entry.sess)
			delete(p.sessions, key)
		}
	}
	p.mu.Unlock()

	// Close outside of lock
	for _, s := range toClose {
		s.Close()
	}
}
