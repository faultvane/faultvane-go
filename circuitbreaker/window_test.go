package circuitbreaker

import (
	"sync"
	"testing"
	"time"
)

func assertTotals(t *testing.T, w *window, elapsed time.Duration, calls, failures, slowCalls int64) {
	t.Helper()
	gotCalls, gotFailures, gotSlow := w.totals(elapsed)
	if gotCalls != calls || gotFailures != failures || gotSlow != slowCalls {
		t.Fatalf("totals(%v) = (%d, %d, %d), want (%d, %d, %d)", elapsed, gotCalls, gotFailures, gotSlow, calls, failures, slowCalls)
	}
}

func TestWindow(t *testing.T) {
	t.Run("FD-3 an empty window has no calls", func(t *testing.T) {
		assertTotals(t, newWindow(10*time.Second, 10), 0, 0, 0, 0)
	})

	t.Run("FD-3 the bucket duration is the window size divided by the buckets", func(t *testing.T) {
		w := newWindow(10*time.Second, 5)
		if w.bucketDuration != 2*time.Second || len(w.slots) != 5 {
			t.Fatalf("bucketDuration = %v, slots = %d, want 2s, 5", w.bucketDuration, len(w.slots))
		}
	})

	t.Run("FD-3 a record counts calls, failures, and slow calls separately", func(t *testing.T) {
		w := newWindow(10*time.Second, 10)
		w.record(0, false, false)
		w.record(0, true, false)
		w.record(0, false, true)
		w.record(0, true, true)
		assertTotals(t, w, 0, 4, 2, 2)
	})

	t.Run("FD-3 records in different buckets add up", func(t *testing.T) {
		w := newWindow(10*time.Second, 10)
		w.record(0, true, false)
		w.record(999*time.Millisecond, true, false)
		w.record(time.Second, false, false)
		w.record(9*time.Second, true, true)
		assertTotals(t, w, 9*time.Second, 4, 3, 1)
		if first, second := w.slots[0].Load().calls.Load(), w.slots[1].Load().calls.Load(); first != 2 || second != 1 {
			t.Fatalf("calls in bucket 0 and 1 = %d, %d, want 2, 1", first, second)
		}
	})

	t.Run("FD-3 a bucket leaves the window after the window size", func(t *testing.T) {
		w := newWindow(10*time.Second, 10)
		w.record(0, true, false)
		w.record(time.Second, false, false)
		assertTotals(t, w, 10*time.Second-time.Nanosecond, 2, 1, 0)
		assertTotals(t, w, 10*time.Second, 1, 0, 0)
		assertTotals(t, w, 11*time.Second, 0, 0, 0)
	})

	t.Run("FD-3 a slot starts again for a later bucket", func(t *testing.T) {
		w := newWindow(10*time.Second, 10)
		w.record(0, true, true)
		w.record(10*time.Second, false, false)
		assertTotals(t, w, 10*time.Second, 1, 0, 0)
	})

	t.Run("FD-3 a late record does not replace a newer bucket", func(t *testing.T) {
		w := newWindow(10*time.Second, 10)
		w.record(10*time.Second, false, false)
		w.record(0, true, false)
		assertTotals(t, w, 10*time.Second, 2, 1, 0)
		if epoch := w.slots[0].Load().epoch; epoch != 10 {
			t.Fatalf("epoch = %d, want 10", epoch)
		}
	})

	t.Run("TK-16 PF-5 the window keeps a fixed number of buckets", func(t *testing.T) {
		w := newWindow(10*time.Second, 10)
		for second := range 1000 {
			w.record(time.Duration(second)*time.Second, true, false)
		}
		if len(w.slots) != 10 {
			t.Fatalf("slots = %d, want 10", len(w.slots))
		}
		assertTotals(t, w, 999*time.Second, 10, 10, 0)
	})

	t.Run("TK-14 concurrent records lose no call", func(t *testing.T) {
		w := newWindow(10*time.Second, 10)
		const goroutines, records = 64, 500
		var group sync.WaitGroup
		begin := make(chan struct{})
		for g := range goroutines {
			group.Add(1)
			go func() {
				defer group.Done()
				<-begin
				for range records {
					w.record(time.Duration(g%10)*time.Second, true, g%2 == 0)
				}
			}()
		}
		close(begin)
		group.Wait()
		assertTotals(t, w, 9*time.Second, goroutines*records, goroutines*records, goroutines*records/2)
	})
}
