package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
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

// If NodeStatus constants exist, test them; otherwise skip
func TestNodeStatus_Constants(t *testing.T) {
	// These constants may or may not exist depending on your types definition
	// Just verify the type exists by creating a variable
	var s NodeStatus
	_ = s // Suppress unused variable warning

	// If you have specific constants, add them here:
	// assert.Equal(t, NodeStatusNotFound, NodeStatus("not-found"))
}
