package sshclient

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/vlad-zapp/sshtmux/internal/vlog"
)

// Session wraps an SSH session's stdio.
type Session interface {
	StdinPipe() (io.WriteCloser, error)
	StdoutPipe() (io.Reader, error)
	StderrPipe() (io.Reader, error)
	Start(cmd string) error
	Wait() error
	Close() error
}

// Client wraps an SSH client connection.
type Client interface {
	NewSession() (Session, error)
	Close() error
}

// Dialer creates SSH connections.
type Dialer interface {
	Dial(ctx context.Context, host, user string) (Client, error)
}

// RealDialer creates SSH connections by spawning the openssh binary.
// This gives full compatibility with ~/.ssh/config (ProxyJump, ProxyCommand,
// Match, Include, IdentityFile, certificates, FIDO keys, etc.).
type RealDialer struct {
	IgnoreHostKeys bool
}

func (d *RealDialer) Dial(ctx context.Context, host, user string) (Client, error) {
	vlog.Logf(ctx, "ssh: dialing host=%q user=%q", host, user)
	return &execClient{
		host:           host,
		user:           user,
		ignoreHostKeys: d.IgnoreHostKeys,
	}, nil
}

// execClient implements Client by spawning ssh processes.
type execClient struct {
	host           string
	user           string
	ignoreHostKeys bool

	mu       sync.Mutex
	sessions []*execSession
}

func (c *execClient) NewSession() (Session, error) {
	s := &execSession{
		host:           c.host,
		user:           c.user,
		ignoreHostKeys: c.ignoreHostKeys,
	}
	c.mu.Lock()
	c.sessions = append(c.sessions, s)
	c.mu.Unlock()
	return s, nil
}

func (c *execClient) Close() error {
	c.mu.Lock()
	sessions := c.sessions
	c.sessions = nil
	c.mu.Unlock()

	for _, s := range sessions {
		s.Close()
	}
	return nil
}

// execSession implements Session by running an ssh process.
type execSession struct {
	host           string
	user           string
	ignoreHostKeys bool

	cmd     *exec.Cmd
	stdinR  *io.PipeReader
	stdoutW *io.PipeWriter
	stderrW *io.PipeWriter
	waitCh  chan error
}

func (s *execSession) StdinPipe() (io.WriteCloser, error) {
	r, w := io.Pipe()
	s.stdinR = r
	return w, nil
}

func (s *execSession) StdoutPipe() (io.Reader, error) {
	r, w := io.Pipe()
	s.stdoutW = w
	return r, nil
}

func (s *execSession) StderrPipe() (io.Reader, error) {
	r, w := io.Pipe()
	s.stderrW = w
	return r, nil
}

func (s *execSession) buildArgs(remoteCmd string) []string {
	args := []string{"-o", "BatchMode=yes"}
	if s.ignoreHostKeys {
		args = append(args, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null")
	}
	if vlog.GetEnabled() {
		args = append(args, "-v")
	}
	if s.user != "" {
		args = append(args, "-l", s.user)
	}
	args = append(args, "--", s.host)
	if remoteCmd != "" {
		args = append(args, remoteCmd)
	}
	return args
}

func (s *execSession) Start(remoteCmd string) error {
	args := s.buildArgs(remoteCmd)
	vlog.Printf("ssh: exec ssh %s", strings.Join(args, " "))
	s.cmd = exec.Command("ssh", args...)

	if s.stdinR != nil {
		s.cmd.Stdin = s.stdinR
	}
	if s.stdoutW != nil {
		s.cmd.Stdout = s.stdoutW
	} else {
		s.cmd.Stdout = os.Stdout
	}
	if s.stderrW != nil {
		s.cmd.Stderr = s.stderrW
	} else {
		s.cmd.Stderr = os.Stderr
	}

	if err := s.cmd.Start(); err != nil {
		return fmt.Errorf("start ssh: %w", err)
	}

	s.waitCh = make(chan error, 1)
	go func() {
		err := s.cmd.Wait()
		if s.stdoutW != nil {
			s.stdoutW.Close()
		}
		if s.stderrW != nil {
			s.stderrW.Close()
		}
		if s.stdinR != nil {
			s.stdinR.Close()
		}
		s.waitCh <- err
	}()

	return nil
}

func (s *execSession) Wait() error {
	if s.waitCh == nil {
		return nil
	}
	return <-s.waitCh
}

func (s *execSession) Close() error {
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
	return nil
}
