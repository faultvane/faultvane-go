package storetest

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/circuitbreaker"
)

const (
	threshold = 5
	long      = time.Hour
	short     = time.Millisecond
)

var nameCounter atomic.Uint64

type recorder struct {
	store        circuitbreaker.Store
	name         string
	patience     time.Duration
	openDuration time.Duration
	lease        time.Duration
	windowSize   time.Duration
	limit        int
	successes    int
	fingerprint  string
	problems     []string
}

func newRecorder(store circuitbreaker.Store, patience time.Duration) *recorder {
	return &recorder{
		store:        store,
		name:         fmt.Sprintf("conformance-%d-%d", time.Now().UnixNano(), nameCounter.Add(1)),
		patience:     patience,
		openDuration: long,
		lease:        long,
		windowSize:   long,
		limit:        1,
		successes:    1,
	}
}

func (r *recorder) problem(format string, args ...any) {
	r.problems = append(r.problems, fmt.Sprintf(format, args...))
}

func (r *recorder) noError(operation string, err error) {
	if err != nil {
		r.problem("%s: error %v", operation, err)
	}
}

func (r *recorder) expect(step string, snapshot circuitbreaker.Snapshot, state circuitbreaker.State, generation uint64) {
	if snapshot.State != state || snapshot.Generation != generation {
		r.problem("%s: state %v generation %d, want %v generation %d", step, snapshot.State, snapshot.Generation, state, generation)
	}
}

func (r *recorder) expectFingerprint(step string, snapshot circuitbreaker.Snapshot, fingerprint string) {
	if snapshot.Fingerprint != fingerprint {
		r.problem("%s: fingerprint %q, want %q", step, snapshot.Fingerprint, fingerprint)
	}
}

func (r *recorder) expectTrue(step string, ok bool) {
	if !ok {
		r.problem("%s", step)
	}
}

func (r *recorder) load() circuitbreaker.Snapshot {
	snapshot, err := r.store.Load(context.Background(), r.name)
	r.noError("Load", err)
	return snapshot
}

func (r *recorder) report(generation uint64, calls, failures int64) circuitbreaker.Snapshot {
	return r.identified(0, 0, generation, calls, failures)
}

func (r *recorder) identified(instance, sequence, generation uint64, calls, failures int64) circuitbreaker.Snapshot {
	snapshot, err := r.store.Report(context.Background(), r.name, circuitbreaker.Report{
		Generation:    generation,
		Instance:      instance,
		Sequence:      sequence,
		Calls:         calls,
		Failures:      failures,
		Thresholds:    circuitbreaker.Thresholds{FailureThreshold: threshold, MinimumCalls: 1 << 40, FailureRate: 1},
		WindowSize:    r.windowSize,
		WindowBuckets: 2,
		OpenDuration:  r.openDuration,
		StateTTL:      long,
		Fingerprint:   r.fingerprint,
	})
	r.noError("Report", err)
	return snapshot
}

func (r *recorder) acquire(token uint64) (circuitbreaker.Snapshot, bool) {
	grant, err := r.store.AcquireProbe(context.Background(), r.name, circuitbreaker.ProbeRequest{
		Token:         token,
		Limit:         r.limit,
		LeaseDuration: r.lease,
		StateTTL:      long,
		Fingerprint:   r.fingerprint,
	})
	r.noError("AcquireProbe", err)
	return grant.Snapshot, grant.Granted
}

func (r *recorder) complete(generation, token uint64, result circuitbreaker.Result) circuitbreaker.Snapshot {
	snapshot, err := r.store.CompleteProbe(context.Background(), r.name, circuitbreaker.ProbeResult{
		Generation:       generation,
		Token:            token,
		Result:           result,
		SuccessThreshold: r.successes,
		OpenDuration:     r.openDuration,
		StateTTL:         long,
		Fingerprint:      r.fingerprint,
	})
	r.noError("CompleteProbe", err)
	return snapshot
}

func (r *recorder) restore(snapshot circuitbreaker.Snapshot) circuitbreaker.Snapshot {
	restored, err := r.store.Restore(context.Background(), r.name, snapshot)
	r.noError("Restore", err)
	return restored
}

func (r *recorder) as(fingerprint string) *recorder {
	r.fingerprint = fingerprint
	return r
}

func (r *recorder) open() {
	r.expect("open the circuit", r.report(0, threshold, threshold), circuitbreaker.Open, 1)
}

func (r *recorder) waitForProbe(token uint64) circuitbreaker.Snapshot {
	deadline := time.Now().Add(r.patience)
	for {
		snapshot, granted := r.acquire(token)
		if granted {
			return snapshot
		}
		if time.Now().After(deadline) {
			r.problem("no probe lease for token %d in %v", token, r.patience)
			return snapshot
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (r *recorder) halfOpen() {
	r.openDuration = short
	r.open()
	r.expect("first probe after the open duration", r.waitForProbe(1), circuitbreaker.HalfOpen, 2)
}

type check struct {
	name string
	run  func(r *recorder)
}

var checks = []check{
	{"C3 a breaker without state is closed with generation 0", func(r *recorder) {
		r.expect("Load", r.load(), circuitbreaker.Closed, 0)
	}},
	{"C3 a report below the threshold keeps the circuit closed", func(r *recorder) {
		r.expect("Report", r.report(0, 10, threshold-1), circuitbreaker.Closed, 0)
		r.expect("Load", r.load(), circuitbreaker.Closed, 0)
	}},
	{"C3 C4 reports of many instances open the circuit at the threshold", func(r *recorder) {
		r.expect("first report", r.report(0, 3, 3), circuitbreaker.Closed, 0)
		r.expect("second report", r.report(0, 2, 2), circuitbreaker.Open, 1)
		r.expect("Load", r.load(), circuitbreaker.Open, 1)
	}},
	{"C4 a report with a different generation adds nothing", func(r *recorder) {
		r.expect("report with a higher generation", r.report(5, 100, 100), circuitbreaker.Closed, 0)
		r.expect("report below the threshold", r.report(0, threshold-1, threshold-1), circuitbreaker.Closed, 0)
		r.expect("report that reaches the threshold", r.report(0, 1, 1), circuitbreaker.Open, 1)
	}},
	{"C3 a report adds nothing when the circuit is not closed", func(r *recorder) {
		r.open()
		r.expect("report in open", r.report(1, 100, 100), circuitbreaker.Open, 1)
	}},
	{"C3 the shared window forgets old buckets", func(r *recorder) {
		r.windowSize = 100 * time.Millisecond
		r.expect("first report", r.report(0, threshold-1, threshold-1), circuitbreaker.Closed, 0)
		time.Sleep(r.windowSize + 60*time.Millisecond)
		r.expect("report after the window", r.report(0, threshold-1, threshold-1), circuitbreaker.Closed, 0)
		r.expect("report that reaches the threshold", r.report(0, 1, 1), circuitbreaker.Open, 1)
	}},
	{"C3 no probe starts before the open duration ends", func(r *recorder) {
		r.open()
		snapshot, granted := r.acquire(1)
		r.expect("AcquireProbe", snapshot, circuitbreaker.Open, 1)
		r.expectTrue("the store granted a probe in open", !granted)
	}},
	{"C3 a closed circuit grants no probe", func(r *recorder) {
		snapshot, granted := r.acquire(1)
		r.expect("AcquireProbe", snapshot, circuitbreaker.Closed, 0)
		r.expectTrue("the store granted a probe in closed", !granted)
	}},
	{"C3 C6 the first probe request after the open duration gets a lease", func(r *recorder) {
		r.halfOpen()
		r.expect("Load", r.load(), circuitbreaker.HalfOpen, 2)
	}},
	{"C6 the store grants no probe above the limit", func(r *recorder) {
		r.halfOpen()
		snapshot, granted := r.acquire(2)
		r.expect("second request with limit 1", snapshot, circuitbreaker.HalfOpen, 2)
		r.expectTrue("the store granted a second probe with limit 1", !granted)
		r.limit = 2
		_, granted = r.acquire(3)
		r.expectTrue("the store granted no second probe with limit 2", granted)
		_, granted = r.acquire(4)
		r.expectTrue("the store granted a third probe with limit 2", !granted)
	}},
	{"C6 a lease ends without an action of its owner", func(r *recorder) {
		r.lease = 40 * time.Millisecond
		r.halfOpen()
		r.expect("probe after the lease of the first owner ended", r.waitForProbe(2), circuitbreaker.HalfOpen, 2)
		r.expect("result of the first owner", r.complete(2, 1, circuitbreaker.Failure), circuitbreaker.HalfOpen, 2)
	}},
	{"C3 a failed probe opens the circuit", func(r *recorder) {
		r.halfOpen()
		r.expect("CompleteProbe", r.complete(2, 1, circuitbreaker.Failure), circuitbreaker.Open, 3)
	}},
	{"C3 a successful probe closes the circuit with an empty window", func(r *recorder) {
		r.halfOpen()
		r.expect("CompleteProbe", r.complete(2, 1, circuitbreaker.Success), circuitbreaker.Closed, 3)
		r.expect("report below the threshold", r.report(3, threshold-1, threshold-1), circuitbreaker.Closed, 3)
	}},
	{"C3 the circuit closes only at the success threshold", func(r *recorder) {
		r.successes = 2
		r.halfOpen()
		r.expect("first success", r.complete(2, 1, circuitbreaker.Success), circuitbreaker.HalfOpen, 2)
		_, granted := r.acquire(2)
		r.expectTrue("no lease after the first success", granted)
		r.expect("second success", r.complete(2, 2, circuitbreaker.Success), circuitbreaker.Closed, 3)
	}},
	{"C3 an ignored probe result gives the lease back", func(r *recorder) {
		r.halfOpen()
		r.expect("CompleteProbe", r.complete(2, 1, circuitbreaker.Ignored), circuitbreaker.HalfOpen, 2)
		_, granted := r.acquire(2)
		r.expectTrue("no lease after an ignored result", granted)
	}},
	{"C4 a probe result with a different generation or an unknown token changes nothing", func(r *recorder) {
		r.halfOpen()
		r.expect("old generation", r.complete(1, 1, circuitbreaker.Success), circuitbreaker.HalfOpen, 2)
		r.expect("unknown token", r.complete(2, 99, circuitbreaker.Failure), circuitbreaker.HalfOpen, 2)
		r.expect("valid result", r.complete(2, 1, circuitbreaker.Success), circuitbreaker.Closed, 3)
	}},
	{"C11 restore writes a snapshot with a higher generation", func(r *recorder) {
		now := r.load().StoreTime
		lost := circuitbreaker.Snapshot{State: circuitbreaker.Open, Generation: 7, TransitionAt: now, OpenDuration: long}
		r.expect("Restore", r.restore(lost), circuitbreaker.Open, 7)
		restored := r.load()
		r.expect("Load", restored, circuitbreaker.Open, 7)
		r.expectTrue("the open duration of the restored state is different", restored.OpenDuration == long)
		r.expectTrue("the transition time of the restored state is different", restored.TransitionAt.Sub(now).Abs() < time.Millisecond)
		snapshot, granted := r.acquire(1)
		r.expect("probe request on the restored open state", snapshot, circuitbreaker.Open, 7)
		r.expectTrue("the store granted a probe before the restored open duration ended", !granted)
	}},
	{"C11 restore with a generation that is not higher changes nothing", func(r *recorder) {
		r.open()
		now := r.load().StoreTime
		r.expect("equal generation", r.restore(circuitbreaker.Snapshot{State: circuitbreaker.Closed, Generation: 1, TransitionAt: now}), circuitbreaker.Open, 1)
		r.expect("lower generation", r.restore(circuitbreaker.Snapshot{State: circuitbreaker.Closed, Generation: 0, TransitionAt: now}), circuitbreaker.Open, 1)
	}},
	{"C11 restore removes the window and the leases", func(r *recorder) {
		r.expect("report", r.report(0, threshold-1, threshold-1), circuitbreaker.Closed, 0)
		now := r.load().StoreTime
		r.expect("Restore", r.restore(circuitbreaker.Snapshot{State: circuitbreaker.Closed, Generation: 3, TransitionAt: now}), circuitbreaker.Closed, 3)
		r.expect("report after restore", r.report(3, threshold-1, threshold-1), circuitbreaker.Closed, 3)
	}},
	{"C13 a breaker without state has no fingerprint", func(r *recorder) {
		r.expectFingerprint("Load", r.load(), "")
		r.expectFingerprint("first report", r.as("a").report(0, 1, 0), "")
	}},
	{"C13 a report saves the fingerprint of the caller and returns the fingerprint before it", func(r *recorder) {
		r.as("a").report(0, 1, 0)
		r.expectFingerprint("report of a different caller", r.as("b").report(0, 1, 0), "a")
		r.expectFingerprint("Load", r.load(), "b")
		r.expectFingerprint("report of the first caller", r.as("a").report(0, 1, 0), "b")
		r.expectFingerprint("report that opens the circuit", r.as("c").report(0, threshold, threshold), "a")
		r.expectFingerprint("Load after the transition", r.load(), "c")
	}},
	{"C13 an operation that the store ignores does not change the fingerprint", func(r *recorder) {
		r.as("a").report(0, 1, 0)
		r.expectFingerprint("report with a different generation", r.as("b").report(5, 1, 0), "a")
		_, _ = r.as("b").acquire(1)
		r.expectFingerprint("Load after a probe request in closed", r.load(), "a")
		r.as("a").open()
		r.expectFingerprint("report in open", r.as("b").report(1, 1, 0), "a")
		_, _ = r.as("b").acquire(1)
		r.as("b").complete(1, 1, circuitbreaker.Success)
		r.expectFingerprint("Load after the ignored operations", r.load(), "a")
	}},
	{"C13 a probe operation saves the fingerprint of the caller and returns the fingerprint before it", func(r *recorder) {
		r.as("a").halfOpen()
		r.expectFingerprint("Load in half-open", r.load(), "a")
		snapshot, granted := r.as("b").acquire(2)
		r.expectTrue("the store granted a second probe with limit 1", !granted)
		r.expectFingerprint("probe request without a grant", snapshot, "a")
		r.expectFingerprint("probe result with a different generation", r.as("c").complete(1, 1, circuitbreaker.Success), "b")
		r.expectFingerprint("probe result", r.as("c").complete(2, 1, circuitbreaker.Success), "b")
		r.expectFingerprint("Load after the probe result", r.load(), "c")
	}},
	{"C13 restore does not change the fingerprint", func(r *recorder) {
		r.as("a").report(0, 1, 0)
		now := r.load().StoreTime
		lost := circuitbreaker.Snapshot{State: circuitbreaker.Open, Generation: 7, TransitionAt: now, OpenDuration: long, Fingerprint: "b"}
		r.expectFingerprint("Restore", r.restore(lost), "a")
		r.expectFingerprint("Load", r.load(), "a")
	}},
	{"C14 a report that the store applied before adds nothing", func(r *recorder) {
		r.expect("first report", r.identified(7, 1, 0, 3, 3), circuitbreaker.Closed, 0)
		r.expect("the same report again", r.identified(7, 1, 0, 3, 3), circuitbreaker.Closed, 0)
		r.expect("next sequence", r.identified(7, 2, 0, 1, 1), circuitbreaker.Closed, 0)
		r.expect("late report with a lower sequence", r.identified(7, 1, 0, 3, 3), circuitbreaker.Closed, 0)
		r.expect("report that reaches the threshold", r.identified(7, 3, 0, 1, 1), circuitbreaker.Open, 1)
	}},
	{"C14 each instance has its own sequence", func(r *recorder) {
		r.expect("first instance", r.identified(7, 1, 0, 3, 3), circuitbreaker.Closed, 0)
		r.expect("second instance with the same sequence", r.identified(1<<63+8, 1, 0, 2, 2), circuitbreaker.Open, 1)
	}},
	{"C14 a report without a sequence is added each time", func(r *recorder) {
		r.expect("report with a sequence", r.identified(0, 1, 0, 1, 1), circuitbreaker.Closed, 0)
		r.expect("first report without a sequence", r.identified(0, 0, 0, 2, 2), circuitbreaker.Closed, 0)
		r.expect("second report without a sequence", r.identified(0, 0, 0, 2, 2), circuitbreaker.Open, 1)
	}},
	{"C14 C13 a report that the store applied before does not change the fingerprint", func(r *recorder) {
		r.as("a").identified(7, 1, 0, 1, 0)
		r.expectFingerprint("the same report again from a different caller", r.as("b").identified(7, 1, 0, 1, 0), "a")
		r.expectFingerprint("Load", r.load(), "a")
	}},
	{"C14 the sequence of an instance continues after a transition", func(r *recorder) {
		r.openDuration = short
		r.expect("report that opens the circuit", r.identified(7, 1, 0, threshold, threshold), circuitbreaker.Open, 1)
		r.expect("probe", r.waitForProbe(1), circuitbreaker.HalfOpen, 2)
		r.expect("probe result", r.complete(2, 1, circuitbreaker.Success), circuitbreaker.Closed, 3)
		r.openDuration = long
		r.expect("next sequence in the new generation", r.identified(7, 2, 3, threshold-1, threshold-1), circuitbreaker.Closed, 3)
		r.expect("the same report again", r.identified(7, 2, 3, threshold-1, threshold-1), circuitbreaker.Closed, 3)
		r.expect("report that reaches the threshold", r.identified(7, 3, 3, 1, 1), circuitbreaker.Open, 4)
	}},
	{"C7 two breakers share no data", func(r *recorder) {
		other := newRecorder(r.store, r.patience)
		r.open()
		other.expect("the other breaker", other.load(), circuitbreaker.Closed, 0)
		other.expect("report of the other breaker", other.report(0, 1, 1), circuitbreaker.Closed, 0)
		r.problems = append(r.problems, other.problems...)
	}},
	{"C5 each response contains a store time that does not go back", func(r *recorder) {
		first := r.load().StoreTime
		second := r.report(0, 1, 0).StoreTime
		r.expectTrue("the store time is zero", !first.IsZero() && !second.IsZero())
		r.expectTrue("the store time went back", !second.Before(first))
	}},
	{"C8 an operation with an ended context returns an error", func(r *recorder) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := r.store.Load(ctx, r.name)
		r.expectTrue("Load returned no error", err != nil)
		_, err = r.store.Report(ctx, r.name, circuitbreaker.Report{WindowSize: long, WindowBuckets: 1})
		r.expectTrue("Report returned no error", err != nil)
		_, err = r.store.AcquireProbe(ctx, r.name, circuitbreaker.ProbeRequest{Token: 1, Limit: 1, LeaseDuration: long})
		r.expectTrue("AcquireProbe returned no error", err != nil)
		_, err = r.store.CompleteProbe(ctx, r.name, circuitbreaker.ProbeResult{Token: 1})
		r.expectTrue("CompleteProbe returned no error", err != nil)
		_, err = r.store.Restore(ctx, r.name, circuitbreaker.Snapshot{Generation: 1})
		r.expectTrue("Restore returned no error", err != nil)
		r.expect("Load after the failed operations", r.load(), circuitbreaker.Closed, 0)
	}},
	{"C1 C6 concurrent probe requests get exactly the limit", func(r *recorder) {
		r.limit = 3
		r.openDuration = short
		r.open()
		time.Sleep(20 * time.Millisecond)
		var granted atomic.Int64
		var group sync.WaitGroup
		for token := uint64(1); token <= 32; token++ {
			group.Add(1)
			go func() {
				defer group.Done()
				grant, err := r.store.AcquireProbe(context.Background(), r.name, circuitbreaker.ProbeRequest{Token: token, Limit: r.limit, LeaseDuration: long, StateTTL: long})
				if err == nil && grant.Granted {
					granted.Add(1)
				}
			}()
		}
		group.Wait()
		r.expectTrue(fmt.Sprintf("%d probes granted, want 3", granted.Load()), granted.Load() == 3)
		r.expect("Load", r.load(), circuitbreaker.HalfOpen, 2)
	}},
	{"C1 C4 concurrent reports open the circuit one time", func(r *recorder) {
		var group sync.WaitGroup
		for range 32 {
			group.Add(1)
			go func() {
				defer group.Done()
				_, _ = r.store.Report(context.Background(), r.name, circuitbreaker.Report{
					Calls:         1,
					Failures:      1,
					Thresholds:    circuitbreaker.Thresholds{FailureThreshold: threshold, MinimumCalls: 1 << 40, FailureRate: 1},
					WindowSize:    long,
					WindowBuckets: 2,
					OpenDuration:  long,
					StateTTL:      long,
				})
			}()
		}
		group.Wait()
		r.expect("Load", r.load(), circuitbreaker.Open, 1)
	}},
	{"C3 the store behaves as the reference for random sequences of operations", func(r *recorder) {
		random := rand.New(rand.NewPCG(7, 11))
		for sequence := range 6 {
			tested := newRecorder(r.store, r.patience)
			reference := newRecorder(circuitbreaker.NewMemoryStore(faultvane.SystemClock()), r.patience)
			for _, side := range []*recorder{tested, reference} {
				side.openDuration, side.limit, side.successes = short, 2, 2
			}
			token := uint64(0)
			for step := range 25 {
				tested.fingerprint = string(rune('a' + random.IntN(3)))
				reference.fingerprint = tested.fingerprint
				generation := reference.load().Generation
				if random.IntN(4) == 0 {
					generation += uint64(random.IntN(3)) + 1
				}
				var got, want circuitbreaker.Snapshot
				gotGranted, wantGranted := false, false
				switch operation := random.IntN(4); operation {
				case 0:
					failures, instance, sequence := int64(random.IntN(4)), uint64(random.IntN(2)), uint64(random.IntN(4))
					got, want = tested.identified(instance, sequence, generation, failures, failures), reference.identified(instance, sequence, generation, failures, failures)
				case 1:
					token++
					got, gotGranted = tested.acquire(token)
					want, wantGranted = reference.acquire(token)
				case 2:
					used, result := uint64(random.IntN(int(token)+2)), circuitbreaker.Result(random.IntN(3))
					got, want = tested.complete(generation, used, result), reference.complete(generation, used, result)
				default:
					lost := circuitbreaker.Snapshot{State: circuitbreaker.State(random.IntN(3)), Generation: generation, OpenDuration: short}
					lost.TransitionAt = tested.load().StoreTime
					got = tested.restore(lost)
					lost.TransitionAt = reference.load().StoreTime
					want = reference.restore(lost)
				}
				if got.State != want.State || got.Generation != want.Generation || gotGranted != wantGranted || got.Fingerprint != want.Fingerprint {
					r.problem("sequence %d step %d: state %v generation %d granted %v fingerprint %q, the reference has %v, %d, %v, %q", sequence, step, got.State, got.Generation, gotGranted, got.Fingerprint, want.State, want.Generation, wantGranted, want.Fingerprint)
				}
				time.Sleep(4 * time.Millisecond)
			}
			r.problems = append(r.problems, tested.problems...)
		}
	}},
}

func (c check) problems(store circuitbreaker.Store, patience time.Duration) []string {
	r := newRecorder(store, patience)
	c.run(r)
	return r.problems
}

func Run(t *testing.T, newStore func(t *testing.T) circuitbreaker.Store) {
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			for _, problem := range c.problems(newStore(t), 10*time.Second) {
				t.Error(problem)
			}
		})
	}
}
