package sshutil

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrivateKeyAuthReportsMissingAndInvalidKeys(t *testing.T) {
	_, err := PrivateKeyAuth("/nonexistent/key")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read private key")

	bad := filepath.Join(t.TempDir(), "k")
	require.NoError(t, os.WriteFile(bad, []byte("not a key"), 0o600))
	_, err = PrivateKeyAuth(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse private key")
}

func TestNewClientDefaults(t *testing.T) {
	c := NewClient("vllm", "192.168.1.50", true)
	assert.Equal(t, "vllm", c.user)
	assert.Equal(t, "192.168.1.50", c.host)
	assert.Equal(t, "22", c.port)
	assert.NotNil(t, c.cfg.HostKeyCallback)
}

func TestRunReportsDialFailure(t *testing.T) {
	c := NewClient("vllm", "127.0.0.1", true)
	c.port = "1" // nothing listens here
	_, err := c.Run(t.Context(), "true")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dial SSH")
}

func TestRunReturnsOutput(t *testing.T) {
	s := newTestServer(t, "hello\n", false)
	out, err := s.client().Run(t.Context(), "echo hello")
	require.NoError(t, err)
	assert.Equal(t, "hello\n", out)
}

func TestRunReturnsPromptlyWhenCancelledMidCommand(t *testing.T) {
	s := newTestServer(t, "", true)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-s.started
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		_, err := s.client().Run(ctx, "sudo podman pull example")
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("Run kept waiting on the remote command after its context was cancelled")
	}
}

func TestRunDoesNotDialWithACancelledContext(t *testing.T) {
	s := newTestServer(t, "", false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := s.client().Run(ctx, "true")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	select {
	case <-s.started:
		t.Fatal("a cancelled Run still sent its command")
	default:
	}
}
