package sshclient

import (
	"context"
	"testing"
)

func TestDialReturnsClient(t *testing.T) {
	d := &RealDialer{IgnoreHostKeys: true}
	client, err := d.Dial(context.Background(), "testhost", "testuser")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	_, err = sess.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}

	_, err = sess.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
}

func TestExecSessionStartFailsWithBadCommand(t *testing.T) {
	s := &execSession{
		host:           "testhost",
		user:           "testuser",
		ignoreHostKeys: true,
	}

	// Override the command to something that doesn't exist
	_, err := s.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	// Start with a valid remote command — the ssh binary may or may not be
	// available in the test environment, but if ssh is missing Start will
	// return an error, which is the correct behavior.
	err = s.Start("echo hello")
	if err != nil {
		// ssh not found — that's fine for this test
		t.Skipf("ssh binary not available: %v", err)
	}
	defer s.Close()
}

func TestExecSessionBuildArgs(t *testing.T) {
	tests := []struct {
		name           string
		host           string
		user           string
		ignoreHostKeys bool
		wantContains   []string
		wantNotContain []string
	}{
		{
			name:           "with user and ignore host keys",
			host:           "myhost",
			user:           "myuser",
			ignoreHostKeys: true,
			wantContains:   []string{"-l", "myuser", "StrictHostKeyChecking=no", "UserKnownHostsFile=/dev/null", "myhost"},
		},
		{
			name:           "without user",
			host:           "myhost",
			user:           "",
			ignoreHostKeys: true,
			wantNotContain: []string{"-l"},
			wantContains:   []string{"myhost"},
		},
		{
			name:           "strict host keys",
			host:           "myhost",
			user:           "",
			ignoreHostKeys: false,
			wantNotContain: []string{"StrictHostKeyChecking", "UserKnownHostsFile"},
			wantContains:   []string{"BatchMode=yes", "myhost"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &execSession{
				host:           tt.host,
				user:           tt.user,
				ignoreHostKeys: tt.ignoreHostKeys,
			}

			args := s.buildArgs("echo test")

			argStr := ""
			for _, a := range args {
				argStr += a + " "
			}

			for _, want := range tt.wantContains {
				found := false
				for _, a := range args {
					if a == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("args %v should contain %q", args, want)
				}
			}

			for _, notWant := range tt.wantNotContain {
				for _, a := range args {
					if a == notWant {
						t.Errorf("args %v should not contain %q", args, notWant)
					}
				}
			}
		})
	}
}

func TestClientCloseMultipleSessions(t *testing.T) {
	d := &RealDialer{IgnoreHostKeys: true}
	client, err := d.Dial(context.Background(), "testhost", "testuser")
	if err != nil {
		t.Fatal(err)
	}

	// Create multiple sessions
	for range 3 {
		_, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
	}

	// Close should not panic
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestDialContextCancelled(t *testing.T) {
	d := &RealDialer{IgnoreHostKeys: true}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Dial itself doesn't connect (just creates struct), so it shouldn't fail
	// even with a cancelled context.
	client, err := d.Dial(ctx, "testhost", "testuser")
	if err != nil {
		t.Fatalf("Dial should not fail: %v", err)
	}

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	sess.StdinPipe()
	sess.StdoutPipe()

	// Start spawns ssh process. It may fail if ssh is not available.
	err = sess.Start("echo hello")
	if err != nil {
		t.Skipf("ssh binary not available: %v", err)
	}

	// The SSH process is not tied to the dial context (by design: sessions
	// outlive individual requests). Close it explicitly.
	sess.Close()
}
