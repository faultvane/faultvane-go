package circuitbreaker

import (
	"sync/atomic"
	"time"
)

type bucket struct {
	epoch     int64
	calls     atomic.Int64
	failures  atomic.Int64
	slowCalls atomic.Int64
}

type window struct {
	bucketDuration time.Duration
	slots          []atomic.Pointer[bucket]
}

func newWindow(size time.Duration, buckets int) *window {
	return &window{
		bucketDuration: size / time.Duration(buckets),
		slots:          make([]atomic.Pointer[bucket], buckets),
	}
}

func (w *window) epoch(elapsed time.Duration) int64 {
	return int64(elapsed / w.bucketDuration)
}

func (w *window) bucketFor(epoch int64) *bucket {
	slot := &w.slots[epoch%int64(len(w.slots))]
	for {
		current := slot.Load()
		if current != nil && current.epoch >= epoch {
			return current
		}
		fresh := &bucket{epoch: epoch}
		if slot.CompareAndSwap(current, fresh) {
			return fresh
		}
	}
}

func (w *window) record(elapsed time.Duration, failed, slow bool) {
	b := w.bucketFor(w.epoch(elapsed))
	b.calls.Add(1)
	if failed {
		b.failures.Add(1)
	}
	if slow {
		b.slowCalls.Add(1)
	}
}

func (w *window) totals(elapsed time.Duration) (calls, failures, slowCalls int64) {
	oldest := w.epoch(elapsed) - int64(len(w.slots)) + 1
	for i := range w.slots {
		b := w.slots[i].Load()
		if b == nil || b.epoch < oldest {
			continue
		}
		calls += b.calls.Load()
		failures += b.failures.Load()
		slowCalls += b.slowCalls.Load()
	}
	return calls, failures, slowCalls
}
