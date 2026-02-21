package sshclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// startTestSSHServer creates an in-process SSH server for testing.
// Returns the listener address and a cleanup function.
func startTestSSHServer(t *testing.T) (string, func()) {
	t.Helper()

	// Generate host key
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}

	config := &ssh.ServerConfig{
		NoClientAuth: true,
	}
	config.AddHostKey(hostSigner)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go handleTestConn(conn, config)
		}
	}()

	return listener.Addr().String(), func() { listener.Close() }
}

func handleTestConn(conn net.Conn, config *ssh.ServerConfig) {
	defer conn.Close()
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		ch, requests, err := newChan.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer ch.Close()
			for req := range requests {
				switch req.Type {
				case "exec":
					if req.WantReply {
						req.Reply(true, nil)
					}
					// Echo back a simple response
					fmt.Fprintf(ch, "ok\n")
					ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
					return
				case "shell":
					if req.WantReply {
						req.Reply(true, nil)
					}
				default:
					if req.WantReply {
						req.Reply(false, nil)
					}
				}
			}
		}()
	}
}

func TestRealDialerWithTestServer(t *testing.T) {
	addr, cleanup := startTestSSHServer(t)
	defer cleanup()

	host, port, _ := net.SplitHostPort(addr)

	config := &ssh.ClientConfig{
		User:            "testuser",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Auth:            []ssh.AuthMethod{},
		Timeout:         5 * time.Second,
	}

	// Directly use ssh.Dial to verify the test server works
	client, err := ssh.Dial("tcp", net.JoinHostPort(host, port), config)
	if err != nil {
		t.Fatalf("ssh.Dial: %v", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()

	out, err := sess.Output("echo hello")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if string(out) != "ok\n" {
		t.Errorf("output = %q, want %q", string(out), "ok\n")
	}
}

func TestResolveHostPort(t *testing.T) {
	// With default config, an unknown host should return itself and port 22
	hostname, port := resolveHostPort("unknownhost12345")
	if port != "22" {
		t.Errorf("port = %q, want %q", port, "22")
	}
	// hostname might be "unknownhost12345" or resolved via ssh_config
	if hostname == "" {
		t.Error("hostname should not be empty")
	}
}

func TestResolveUser(t *testing.T) {
	user := resolveUser("unknownhost12345")
	if user == "" {
		t.Error("user should not be empty")
	}
}

func TestDialContextCancelled(t *testing.T) {
	d := &RealDialer{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := d.Dial(ctx, "127.0.0.1:1", "testuser")
	if err == nil {
		t.Error("expected error with cancelled context")
	}
}
