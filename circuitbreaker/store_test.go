package circuitbreaker

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go/faultvanetest"
)

func testReport(generation uint64, calls, failures, slowCalls int64) Report {
	return Report{
		Generation:    generation,
		Calls:         calls,
		Failures:      failures,
		SlowCalls:     slowCalls,
		Thresholds:    Thresholds{FailureThreshold: 5, MinimumCalls: 10, FailureRate: 0.5, SlowCallRate: 0.5},
		WindowSize:    10 * time.Second,
		WindowBuckets: 10,
		OpenDuration:  testOpenDuration,
	}
}

func TestRecordReport(t *testing.T) {
	epoch := testStart.UnixNano() / int64(time.Second)

	t.Run("FD-3 a report adds its counts to the bucket of the store time", func(t *testing.T) {
		r := Record{}.Report(testStart, testReport(0, 3, 1, 2))
		r = r.Report(testStart.Add(999*time.Millisecond), testReport(0, 2, 1, 0))
		want := []Bucket{{Epoch: epoch, Calls: 5, Failures: 2, SlowCalls: 2}}
		if !reflect.DeepEqual(r.Buckets, want) || r.State != Closed || r.Generation != 0 {
			t.Fatalf("record = %+v, want the buckets %+v", r, want)
		}
	})

	t.Run("FD-3 a report in a later bucket adds a bucket and removes the buckets that left the window", func(t *testing.T) {
		r := Record{}.Report(testStart, testReport(0, 1, 1, 0))
		r = r.Report(testStart.Add(time.Second), testReport(0, 1, 1, 0))
		r = r.Report(testStart.Add(10*time.Second), testReport(0, 1, 1, 0))
		want := []Bucket{{Epoch: epoch + 1, Calls: 1, Failures: 1}, {Epoch: epoch + 10, Calls: 1, Failures: 1}}
		if !reflect.DeepEqual(r.Buckets, want) {
			t.Fatalf("buckets = %+v, want %+v", r.Buckets, want)
		}
	})

	t.Run("FD-6 SM-2 T1 the sum of the window and the report opens the circuit", func(t *testing.T) {
		r := Record{}.Report(testStart, testReport(0, 2, 2, 0))
		r = r.Report(testStart.Add(9*time.Second), testReport(0, 2, 2, 0))
		if r.State != Closed {
			t.Fatalf("State = %v, want closed with 4 failures", r.State)
		}
		at := testStart.Add(9*time.Second + time.Millisecond)
		r = r.Report(at, testReport(0, 1, 1, 0))
		want := Record{State: Open, Generation: 1, TransitionAt: at, OpenDuration: testOpenDuration}
		if !reflect.DeepEqual(r, want) {
			t.Fatalf("record = %+v, want %+v", r, want)
		}
	})

	t.Run("FD-3 failures that left the window do not open the circuit", func(t *testing.T) {
		r := Record{}.Report(testStart, testReport(0, 4, 4, 0))
		r = r.Report(testStart.Add(10*time.Second), testReport(0, 4, 4, 0))
		if r.State != Closed || len(r.Buckets) != 1 {
			t.Fatalf("record = %+v, want closed with 1 bucket", r)
		}
	})

	t.Run("FD-2 FD-4 the rate conditions use the totals of the window", func(t *testing.T) {
		r := Record{}.Report(testStart, testReport(0, 9, 0, 4))
		if r.State != Closed {
			t.Fatalf("State = %v, want closed below the minimum number of calls", r.State)
		}
		if r = r.Report(testStart, testReport(0, 1, 0, 1)); r.State != Open {
			t.Fatalf("State = %v, want open at a slow call rate of 0.5", r.State)
		}
	})

	t.Run("GEN-3 a report with a different generation changes nothing", func(t *testing.T) {
		r := Record{Generation: 3}
		if got := r.Report(testStart, testReport(2, 100, 100, 0)); !reflect.DeepEqual(got, r) {
			t.Fatalf("record = %+v", got)
		}
		if got := r.Report(testStart, testReport(4, 100, 100, 0)); !reflect.DeepEqual(got, r) {
			t.Fatalf("record = %+v", got)
		}
	})

	t.Run("SM-7 a report changes nothing when the circuit is not closed", func(t *testing.T) {
		for _, state := range []State{Open, HalfOpen} {
			r := Record{State: state, Generation: 1}
			if got := r.Report(testStart, testReport(1, 100, 100, 0)); !reflect.DeepEqual(got, r) {
				t.Fatalf("record = %+v", got)
			}
		}
	})

	t.Run("SM-8 a report does not change the record that it started from", func(t *testing.T) {
		r := Record{}.Report(testStart, testReport(0, 1, 1, 0))
		_ = r.Report(testStart, testReport(0, 1, 1, 0))
		if r.Buckets[0].Calls != 1 {
			t.Fatalf("calls = %d, want 1", r.Buckets[0].Calls)
		}
	})
}

func TestRecordOperations(t *testing.T) {
	request := ProbeRequest{Token: 7, Limit: 1, LeaseDuration: testLease}

	t.Run("HO-2 AcquireProbe uses the values of the request", func(t *testing.T) {
		open := Record{}.trip(0, testStart, testOpenDuration)
		at := testStart.Add(testOpenDuration)
		r, granted := open.AcquireProbe(at, request)
		want := Record{State: HalfOpen, Generation: 2, TransitionAt: at, Leases: []Lease{{Token: 7, ExpiresAt: at.Add(testLease)}}}
		if !granted || !reflect.DeepEqual(r, want) {
			t.Fatalf("granted = %v, record = %+v, want %+v", granted, r, want)
		}
		if _, again := r.AcquireProbe(at, ProbeRequest{Token: 8, Limit: 1, LeaseDuration: testLease}); again {
			t.Fatal("the record granted a probe above the limit")
		}
	})

	t.Run("SM-6 CompleteProbe uses the values of the result", func(t *testing.T) {
		at := testStart.Add(testOpenDuration)
		halfOpen, _ := Record{}.trip(0, testStart, testOpenDuration).AcquireProbe(at, request)
		closed := halfOpen.CompleteProbe(at, ProbeResult{Generation: 2, Token: 7, Result: Success, SuccessThreshold: 1})
		if closed.State != Closed || closed.Generation != 3 {
			t.Fatalf("record = %+v, want closed generation 3", closed)
		}
		kept := halfOpen.CompleteProbe(at, ProbeResult{Generation: 2, Token: 7, Result: Success, SuccessThreshold: 2})
		if kept.State != HalfOpen || kept.ProbeSuccesses != 1 {
			t.Fatalf("record = %+v, want half_open with 1 success", kept)
		}
		opened := halfOpen.CompleteProbe(at, ProbeResult{Generation: 2, Token: 7, Result: Failure, SuccessThreshold: 1, OpenDuration: time.Minute})
		if opened.State != Open || opened.Generation != 3 || opened.OpenDuration != time.Minute {
			t.Fatalf("record = %+v, want open generation 3 for 1m", opened)
		}
		if stale := halfOpen.CompleteProbe(at, ProbeResult{Generation: 1, Token: 7, Result: Success, SuccessThreshold: 1}); stale.State != HalfOpen {
			t.Fatalf("record = %+v, want half_open", stale)
		}
	})

	t.Run("RF-5 Restore writes only a snapshot with a higher generation", func(t *testing.T) {
		current := Record{State: Closed, Generation: 4, Buckets: []Bucket{{Epoch: 1, Calls: 1}}, Leases: []Lease{{Token: 1}}, ProbeSuccesses: 1}
		lost := Snapshot{State: Open, Generation: 5, TransitionAt: testStart, OpenDuration: testOpenDuration, StoreTime: testStart.Add(time.Hour)}
		want := Record{State: Open, Generation: 5, TransitionAt: testStart, OpenDuration: testOpenDuration}
		if got := current.Restore(lost); !reflect.DeepEqual(got, want) {
			t.Fatalf("record = %+v, want %+v", got, want)
		}
		lost.Generation = 4
		if got := current.Restore(lost); !reflect.DeepEqual(got, current) {
			t.Fatalf("record = %+v, want no change for an equal generation", got)
		}
		lost.Generation = 3
		if got := current.Restore(lost); !reflect.DeepEqual(got, current) {
			t.Fatalf("record = %+v, want no change for a lower generation", got)
		}
	})

	t.Run("DS-2 Snapshot contains the state and the store time", func(t *testing.T) {
		r := Record{State: Open, Generation: 9, TransitionAt: testStart, OpenDuration: testOpenDuration, ProbeSuccesses: 1}
		now := testStart.Add(time.Second)
		want := Snapshot{State: Open, Generation: 9, TransitionAt: testStart, OpenDuration: testOpenDuration, StoreTime: now}
		if got := r.Snapshot(now); got != want {
			t.Fatalf("Snapshot() = %+v, want %+v", got, want)
		}
	})
}

func TestMemoryStore(t *testing.T) {
	clock := faultvanetest.NewClock(testStart)

	t.Run("DS-5 each operation returns the state after its work and the store time", func(t *testing.T) {
		store := NewMemoryStore(clock)
		ctx := context.Background()
		snapshot, err := store.Load(ctx, "a")
		if err != nil || snapshot != (Snapshot{StoreTime: testStart}) {
			t.Fatalf("Load() = %+v, %v", snapshot, err)
		}
		snapshot, err = store.Report(ctx, "a", testReport(0, 5, 5, 0))
		want := Snapshot{State: Open, Generation: 1, TransitionAt: testStart, OpenDuration: testOpenDuration, StoreTime: testStart}
		if err != nil || snapshot != want {
			t.Fatalf("Report() = %+v, %v, want %+v", snapshot, err, want)
		}
		grant, err := store.AcquireProbe(ctx, "a", ProbeRequest{Token: 1, Limit: 1, LeaseDuration: testLease})
		if err != nil || grant.Granted || grant.Snapshot != want {
			t.Fatalf("AcquireProbe() = %+v, %v", grant, err)
		}
		restored, err := store.Restore(ctx, "a", Snapshot{State: HalfOpen, Generation: 6, TransitionAt: testStart})
		if err != nil || restored.State != HalfOpen || restored.Generation != 6 {
			t.Fatalf("Restore() = %+v, %v", restored, err)
		}
		grant, err = store.AcquireProbe(ctx, "a", ProbeRequest{Token: 1, Limit: 1, LeaseDuration: testLease})
		if err != nil || !grant.Granted {
			t.Fatalf("AcquireProbe() = %+v, %v, want a lease", grant, err)
		}
		closed, err := store.CompleteProbe(ctx, "a", ProbeResult{Generation: 6, Token: 1, Result: Success, SuccessThreshold: 1})
		if err != nil || closed.State != Closed || closed.Generation != 7 {
			t.Fatalf("CompleteProbe() = %+v, %v", closed, err)
		}
	})

	t.Run("KEY-2 two names have separate records and Delete removes one", func(t *testing.T) {
		store := NewMemoryStore(clock)
		ctx := context.Background()
		_, _ = store.Report(ctx, "a", testReport(0, 5, 5, 0))
		if other, _ := store.Load(ctx, "b"); other.State != Closed || other.Generation != 0 {
			t.Fatalf("Load(b) = %+v", other)
		}
		_, _ = store.Report(ctx, "b", testReport(0, 5, 5, 0))
		store.Delete("a")
		first, _ := store.Load(ctx, "a")
		second, _ := store.Load(ctx, "b")
		if first.Generation != 0 || second.Generation != 1 {
			t.Fatalf("generations = %d, %d, want 0, 1", first.Generation, second.Generation)
		}
	})

	t.Run("API-3 an operation with an ended context returns its error and changes nothing", func(t *testing.T) {
		store := NewMemoryStore(clock)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := store.Report(ctx, "a", testReport(0, 5, 5, 0)); err != context.Canceled {
			t.Fatalf("Report() error = %v, want context.Canceled", err)
		}
		if grant, err := store.AcquireProbe(ctx, "a", ProbeRequest{}); err != context.Canceled || grant.Granted {
			t.Fatalf("AcquireProbe() = %+v, %v", grant, err)
		}
		if snapshot, _ := store.Load(context.Background(), "a"); snapshot.Generation != 0 {
			t.Fatalf("Load() = %+v", snapshot)
		}
	})
}
