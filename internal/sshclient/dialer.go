package sshclient

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	sshconfig "github.com/kevinburke/ssh_config"
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

// RealClient wraps *ssh.Client to implement the Client interface.
type RealClient struct {
	*ssh.Client
}

func (c *RealClient) NewSession() (Session, error) {
	return c.Client.NewSession()
}

// RealDialer connects to real SSH servers using ssh_config and ssh-agent.
type RealDialer struct {
	IgnoreHostKeys bool
}

func (d *RealDialer) Dial(ctx context.Context, host, user string) (Client, error) {
	hostname, port := resolveHostPort(host)

	if user == "" {
		user = resolveUser(host)
	}

	authMethods := buildAuthMethods()
	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no SSH authentication methods available")
	}

	var hostKeyCallback ssh.HostKeyCallback
	if d.IgnoreHostKeys {
		hostKeyCallback = ssh.InsecureIgnoreHostKey()
	} else {
		var err error
		hostKeyCallback, err = buildHostKeyCallback()
		if err != nil {
			return nil, fmt.Errorf("host key callback: %w", err)
		}
	}

	config := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         10 * time.Second,
	}

	addr := net.JoinHostPort(hostname, port)

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	// Use the raw connection to establish SSH
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ssh handshake %s: %w", addr, err)
	}

	client := ssh.NewClient(sshConn, chans, reqs)
	return &RealClient{client}, nil
}

func resolveHostPort(host string) (string, string) {
	hostname := sshconfig.Get(host, "Hostname")
	if hostname == "" {
		hostname = host
	}
	port := sshconfig.Get(host, "Port")
	if port == "" {
		port = "22"
	}
	return hostname, port
}

func resolveUser(host string) string {
	user := sshconfig.Get(host, "User")
	if user != "" {
		return user
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "root"
}

func buildAuthMethods() []ssh.AuthMethod {
	var methods []ssh.AuthMethod

	// Try SSH agent first
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			agentClient := agent.NewClient(conn)
			methods = append(methods, ssh.PublicKeysCallback(agentClient.Signers))
		}
	}

	// Try common key files
	keyFiles := []string{
		"id_ed25519",
		"id_rsa",
		"id_ecdsa",
	}
	home, err := os.UserHomeDir()
	if err == nil {
		for _, kf := range keyFiles {
			path := filepath.Join(home, ".ssh", kf)
			if key, err := loadPrivateKey(path); err == nil {
				methods = append(methods, key)
			}
		}
	}

	return methods
}

func loadPrivateKey(path string) (ssh.AuthMethod, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, err
	}
	return ssh.PublicKeys(signer), nil
}

func buildHostKeyCallback() (ssh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Printf("WARNING: could not determine home directory: %v — host key verification disabled", err)
		return ssh.InsecureIgnoreHostKey(), nil
	}

	knownHostsPath := filepath.Join(home, ".ssh", "known_hosts")
	if _, err := os.Stat(knownHostsPath); os.IsNotExist(err) {
		log.Printf("WARNING: %s not found — host key verification disabled", knownHostsPath)
		return ssh.InsecureIgnoreHostKey(), nil
	}

	callback, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("parse known_hosts: %w", err)
	}
	return callback, nil
}
