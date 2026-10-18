package talos

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jdwlabs/infrastructure/bootstrap/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckTalosctlVersion(t *testing.T) {
	tests := []struct {
		name    string
		want    string
		have    string
		wantErr bool
	}{
		{"equal", "v1.14.4", "v1.14.4", false},
		{"client newer patch", "v1.14.4", "v1.14.5", false},
		{"client newer minor", "v1.13.4", "v1.14.4", false},
		{"unprefixed pin", "1.14.4", "v1.14.4", false},
		{"pin newer minor", "v1.14.4", "v1.13.8", true},
		{"pin newer patch", "v1.14.4", "v1.14.3", true},
		{"pin newer major", "v2.0.0", "v1.14.4", true},
		{"prerelease client of same version", "v1.14.0", "v1.14.0-alpha.1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkTalosctlVersion(tt.want, tt.have)
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.have)
			assert.Contains(t, err.Error(), "v"+strings.TrimPrefix(tt.want, "v"))
		})
	}

	t.Run("unparseable pin", func(t *testing.T) {
		assert.Error(t, checkTalosctlVersion("not-a-version", "v1.14.4"))
	})
}

func TestParseTalosctlClientVersion(t *testing.T) {
	short := "Client:\n\tTalos v1.13.8\n"
	full := "Client:\n\tTag:         v1.14.4\n\tSHA:         abcdef\n\tGo version:  go1.26.0\n"

	got, err := parseTalosctlClientVersion(short)
	require.NoError(t, err)
	assert.Equal(t, "v1.13.8", got)

	got, err = parseTalosctlClientVersion(full)
	require.NoError(t, err)
	assert.Equal(t, "v1.14.4", got)

	_, err = parseTalosctlClientVersion("command not found")
	assert.Error(t, err)
}

// fakeTalosctl installs a talosctl on PATH that reports the given client
// version and records every other invocation, so a test can prove generation
// never started.
func fakeTalosctl(t *testing.T, clientVersion string) (invocations string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub talosctl is unix-only")
	}
	dir := t.TempDir()
	invocations = filepath.Join(dir, "invocations.log")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = version ]; then printf 'Client:\\n\\tTalos " + clientVersion + "\\n'; exit 0; fi\n" +
		"echo \"$@\" >> " + invocations + "\n" +
		"exit 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "talosctl"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return invocations
}

func TestGenerateRefusesTalosctlOlderThanPin(t *testing.T) {
	invocations := fakeTalosctl(t, "v1.13.8")

	cfg := types.TestConfig()
	cfg.TalosVersion = "v1.14.4"
	cfg.SecretsDir = filepath.Join(t.TempDir(), "secrets")
	nc := NewNodeConfig(cfg)

	t.Run("GenerateBaseConfigs", func(t *testing.T) {
		err := nc.GenerateBaseConfigs()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "v1.14.4")
		assert.Contains(t, err.Error(), "v1.13.8")
	})

	t.Run("Generate", func(t *testing.T) {
		_, err := nc.Generate(&types.NodeSpec{VMID: 201, Name: "n", Role: types.RoleControlPlane}, t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "v1.14.4")
		assert.Contains(t, err.Error(), "v1.13.8")
	})

	_, statErr := os.Stat(invocations)
	assert.True(t, os.IsNotExist(statErr), "no generation command may run once the version check fails")
}
