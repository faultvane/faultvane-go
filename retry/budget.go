package retry

import (
	"math"
	"sync/atomic"
)

const (
	unitsPerToken = 1_000_000_000
	budgetCap     = 10 * unitsPerToken
)

type budget struct {
	units        atomic.Int64
	unitsPerCall int64
}

func newBudget(ratio float64) *budget {
	b := &budget{unitsPerCall: max(1, int64(math.Round(ratio*unitsPerToken)))}
	b.units.Store(budgetCap)
	return b
}

func (b *budget) deposit() {
	if b == nil {
		return
	}
	for {
		current := b.units.Load()
		if current == budgetCap || b.units.CompareAndSwap(current, min(budgetCap, current+b.unitsPerCall)) {
			return
		}
	}
}

func (b *budget) withdraw() bool {
	if b == nil {
		return true
	}
	for {
		current := b.units.Load()
		if current < unitsPerToken {
			return false
		}
		if b.units.CompareAndSwap(current, current-unitsPerToken) {
			return true
		}
	}
}
