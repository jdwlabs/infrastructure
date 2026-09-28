package sshutil

import (
	"os"
	"path/filepath"
	"testing"

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
