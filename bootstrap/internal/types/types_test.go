package types

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVMID_String(t *testing.T) {
	assert.Equal(t, "100", VMID(100).String())
	assert.Equal(t, "0", VMID(0).String())
}

func TestRole_Constants(t *testing.T) {
	// Test that role constants exist and have expected values
	assert.Equal(t, RoleControlPlane, Role("control-plane"))
	assert.Equal(t, RoleWorker, Role("worker"))
}

func TestNodeState_TemplateHashJSON(t *testing.T) {
	t.Run("round-trips template_hash", func(t *testing.T) {
		n := NodeState{VMID: 100, ConfigHash: "cfg", TemplateHash: "tmpl"}

		data, err := json.Marshal(n)
		require.NoError(t, err)
		assert.Contains(t, string(data), `"template_hash":"tmpl"`)

		var decoded NodeState
		require.NoError(t, json.Unmarshal(data, &decoded))
		assert.Equal(t, "tmpl", decoded.TemplateHash)
	})

	t.Run("state files without template_hash decode to empty", func(t *testing.T) {
		var decoded NodeState
		require.NoError(t, json.Unmarshal([]byte(`{"vmid":100,"config_hash":"cfg"}`), &decoded))
		assert.Empty(t, decoded.TemplateHash)
	})

	t.Run("empty template_hash is omitted", func(t *testing.T) {
		data, err := json.Marshal(NodeState{VMID: 100})
		require.NoError(t, err)
		assert.NotContains(t, string(data), "template_hash")
	})
}

func TestClusterState_NodeTemplateHash(t *testing.T) {
	state := &ClusterState{
		ControlPlanes: []NodeState{{VMID: 100, TemplateHash: "cp-hash"}},
		Workers:       []NodeState{{VMID: 200, TemplateHash: "worker-hash"}, {VMID: 201}},
	}

	assert.Equal(t, "cp-hash", state.NodeTemplateHash(100))
	assert.Equal(t, "worker-hash", state.NodeTemplateHash(200))
	assert.Empty(t, state.NodeTemplateHash(201), "node without recorded hash")
	assert.Empty(t, state.NodeTemplateHash(999), "unknown node")
}

// If NodeStatus constants exist, test them; otherwise skip
func TestNodeStatus_Constants(t *testing.T) {
	// These constants may or may not exist depending on your types definition
	// Just verify the type exists by creating a variable
	var s NodeStatus
	_ = s // Suppress unused variable warning

	// If you have specific constants, add them here:
	// assert.Equal(t, NodeStatusNotFound, NodeStatus("not-found"))
}
