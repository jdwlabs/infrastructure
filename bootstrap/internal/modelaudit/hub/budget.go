// Package hub reads the Hugging Face Hub and GitHub raw content under one
// shared request budget. It never writes.
package hub

import (
	"errors"
	"sync"
)

// Phase says which part of the budget a request draws on.
type Phase int

const (
	Discovery Phase = iota
	Enrichment
)

// ErrBudget is returned instead of sending a request the budget cannot pay for.
var ErrBudget = errors.New("request budget spent")

// Budget counts every HTTP attempt, retries included. Discovery may spend at
// most its own share; enrichment spends whatever discovery left.
type Budget struct {
	mu            sync.Mutex
	max           int
	discoveryMax  int
	used          int
	discoveryUsed int
}

func NewBudget(max, discovery int) *Budget {
	return &Budget{max: max, discoveryMax: discovery}
}

func (b *Budget) take(p Phase) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used >= b.max || (p == Discovery && b.discoveryUsed >= b.discoveryMax) {
		return ErrBudget
	}
	b.used++
	if p == Discovery {
		b.discoveryUsed++
	}
	return nil
}

// Used is how many attempts have been paid for so far.
func (b *Budget) Used() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}
