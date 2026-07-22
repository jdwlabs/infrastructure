package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConfigRegenReason(t *testing.T) {
	tests := []struct {
		name         string
		configExists bool
		storedHash   string
		currentHash  string
		wantReason   string
	}{
		{
			name:         "missing config always regenerates",
			configExists: false,
			storedHash:   "abc",
			currentHash:  "abc",
			wantReason:   "config file missing",
		},
		{
			name:         "unknown stored hash regenerates without forcing apply",
			configExists: true,
			storedHash:   "",
			currentHash:  "abc",
			wantReason:   "template hash not yet tracked in state",
		},
		{
			name:         "template inputs changed regenerates",
			configExists: true,
			storedHash:   "abc",
			currentHash:  "def",
			wantReason:   "template inputs changed",
		},
		{
			name:         "matching hashes skip regeneration",
			configExists: true,
			storedHash:   "abc",
			currentHash:  "abc",
			wantReason:   "",
		},
		{
			name:         "uncomputable current hash falls back to on-disk comparison",
			configExists: true,
			storedHash:   "abc",
			currentHash:  "",
			wantReason:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := configRegenReason(tt.configExists, tt.storedHash, tt.currentHash)
			assert.Equal(t, tt.wantReason, reason)
		})
	}
}
