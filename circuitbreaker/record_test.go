package circuitbreaker

import (
	"math/rand/v2"
	"reflect"
	"testing"
	"time"
)

const (
	testOpenDuration = 30 * time.Second
	testLease        = 10 * time.Second
)

var testStart = time.Unix(1_000_000, 0)

func openMachine(t *testing.T) Record {
	t.Helper()
	m := Record{}.trip(0, testStart, testOpenDuration)
	if m.State != Open {
		t.Fatalf("state = %v, want open", m.State)
	}
	return m
}

func halfOpenMachine(t *testing.T, limit int) (Record, time.Time) {
	t.Helper()
	now := testStart.Add(testOpenDuration)
	m, granted := openMachine(t).requestProbe(1, now, limit, testLease)
	if m.State != HalfOpen || !granted {
		t.Fatalf("state = %v, granted = %v, want half_open, true", m.State, granted)
	}
	return m, now
}

func assertUnchanged(t *testing.T, before, after Record) {
	t.Helper()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("machine changed from %+v to %+v", before, after)
	}
}

func TestStateString(t *testing.T) {
	cases := map[State]string{Closed: "closed", Open: "open", HalfOpen: "half_open", State(9): "unknown"}
	for state, want := range cases {
		if got := state.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", state, got, want)
		}
	}
}

func TestMachineTrip(t *testing.T) {
	t.Run("SM-1 the zero machine is closed with generation 0", func(t *testing.T) {
		if m := (Record{}); m.State != Closed || m.Generation != 0 {
			t.Fatalf("machine = %+v", m)
		}
	})

	t.Run("SM-2 T1 a trip opens a closed machine", func(t *testing.T) {
		m := Record{}.trip(0, testStart, testOpenDuration)
		want := Record{State: Open, Generation: 1, TransitionAt: testStart, OpenDuration: testOpenDuration}
		assertUnchanged(t, want, m)
		if until := m.openUntil(); !until.Equal(testStart.Add(testOpenDuration)) {
			t.Fatalf("openUntil = %v", until)
		}
	})

	t.Run("GEN-3 G5 a trip with an old generation changes nothing", func(t *testing.T) {
		m := Record{Generation: 4}
		assertUnchanged(t, m, m.trip(3, testStart, testOpenDuration))
		assertUnchanged(t, m, m.trip(5, testStart, testOpenDuration))
	})

	t.Run("SM-7 a trip changes nothing when the machine is not closed", func(t *testing.T) {
		open := openMachine(t)
		assertUnchanged(t, open, open.trip(open.Generation, testStart.Add(time.Second), time.Hour))
		halfOpen, now := halfOpenMachine(t, 1)
		assertUnchanged(t, halfOpen, halfOpen.trip(halfOpen.Generation, now, time.Hour))
	})
}

func TestMachineRequestProbe(t *testing.T) {
	t.Run("SM-7 a closed machine grants no probe", func(t *testing.T) {
		m, granted := Record{}.requestProbe(1, testStart, 1, testLease)
		if granted {
			t.Fatal("granted = true")
		}
		assertUnchanged(t, Record{}, m)
	})

	t.Run("SM-3 an open machine grants no probe before the open duration ends", func(t *testing.T) {
		open := openMachine(t)
		m, granted := open.requestProbe(1, testStart.Add(testOpenDuration-time.Nanosecond), 1, testLease)
		if granted {
			t.Fatal("granted = true")
		}
		assertUnchanged(t, open, m)
	})

	t.Run("SM-4 T2 the first probe request at the end of the open duration gives half-open", func(t *testing.T) {
		m, now := halfOpenMachine(t, 1)
		want := Record{State: HalfOpen, Generation: 2, TransitionAt: now, Leases: []Lease{{Token: 1, ExpiresAt: now.Add(testLease)}}}
		assertUnchanged(t, want, m)
	})

	t.Run("SM-5 HO-4 a half-open machine grants no probe above the limit", func(t *testing.T) {
		m, now := halfOpenMachine(t, 1)
		next, granted := m.requestProbe(2, now.Add(time.Second), 1, testLease)
		if granted || next.Generation != m.Generation || len(next.Leases) != 1 || next.Leases[0].Token != 1 {
			t.Fatalf("granted = %v, machine = %+v", granted, next)
		}
	})

	t.Run("SM-5 a half-open machine grants probes up to the limit", func(t *testing.T) {
		m, now := halfOpenMachine(t, 2)
		m, second := m.requestProbe(2, now, 2, testLease)
		_, third := m.requestProbe(3, now, 2, testLease)
		if !second || third || len(m.Leases) != 2 {
			t.Fatalf("second = %v, third = %v, leases = %d, want true, false, 2", second, third, len(m.Leases))
		}
	})

	t.Run("HO-3 a lease that ended gives its place to a new probe", func(t *testing.T) {
		m, now := halfOpenMachine(t, 1)
		_, early := m.requestProbe(2, now.Add(testLease-time.Nanosecond), 1, testLease)
		next, atEnd := m.requestProbe(2, now.Add(testLease), 1, testLease)
		if early || !atEnd || len(next.Leases) != 1 || next.Leases[0].Token != 2 || next.Generation != m.Generation {
			t.Fatalf("early = %v, atEnd = %v, machine = %+v", early, atEnd, next)
		}
	})

	t.Run("SM-8 a probe request does not change the machine that it started from", func(t *testing.T) {
		m, now := halfOpenMachine(t, 2)
		_, _ = m.requestProbe(2, now, 2, testLease)
		if len(m.Leases) != 1 {
			t.Fatalf("leases = %d, want 1", len(m.Leases))
		}
	})
}

func TestMachineCompleteProbe(t *testing.T) {
	t.Run("SM-7 a probe result changes nothing when the machine is not half-open", func(t *testing.T) {
		open := openMachine(t)
		assertUnchanged(t, open, open.completeProbe(open.Generation, 1, Success, testStart, 1, testOpenDuration))
		assertUnchanged(t, Record{}, Record{}.completeProbe(0, 1, Failure, testStart, 1, testOpenDuration))
	})

	t.Run("GEN-3 G4 a probe result with an old generation changes nothing", func(t *testing.T) {
		m, now := halfOpenMachine(t, 1)
		assertUnchanged(t, m, m.completeProbe(m.Generation-1, 1, Success, now, 1, testOpenDuration))
		assertUnchanged(t, m, m.completeProbe(m.Generation+1, 1, Failure, now, 1, testOpenDuration))
	})

	t.Run("HO-4 a probe result with an unknown token changes nothing", func(t *testing.T) {
		m, now := halfOpenMachine(t, 1)
		next := m.completeProbe(m.Generation, 99, Failure, now, 1, testOpenDuration)
		if next.State != HalfOpen || next.Generation != m.Generation || len(next.Leases) != 1 {
			t.Fatalf("machine = %+v", next)
		}
	})

	t.Run("HO-3 a probe result after the end of its lease changes nothing", func(t *testing.T) {
		m, now := halfOpenMachine(t, 1)
		next := m.completeProbe(m.Generation, 1, Success, now.Add(testLease), 1, testOpenDuration)
		if next.State != HalfOpen || next.Generation != m.Generation || len(next.Leases) != 0 || next.ProbeSuccesses != 0 {
			t.Fatalf("machine = %+v", next)
		}
	})

	t.Run("SM-6 T4 a failed probe opens the machine", func(t *testing.T) {
		m, now := halfOpenMachine(t, 1)
		at := now.Add(time.Second)
		next := m.completeProbe(m.Generation, 1, Failure, at, 1, time.Minute)
		want := Record{State: Open, Generation: 3, TransitionAt: at, OpenDuration: time.Minute}
		assertUnchanged(t, want, next)
	})

	t.Run("SM-6 T3 a successful probe closes the machine at the success threshold", func(t *testing.T) {
		m, now := halfOpenMachine(t, 1)
		at := now.Add(time.Second)
		next := m.completeProbe(m.Generation, 1, Success, at, 1, testOpenDuration)
		want := Record{State: Closed, Generation: 3, TransitionAt: at}
		assertUnchanged(t, want, next)
	})

	t.Run("SM-6 a successful probe below the success threshold keeps half-open", func(t *testing.T) {
		m, now := halfOpenMachine(t, 1)
		next := m.completeProbe(m.Generation, 1, Success, now, 2, testOpenDuration)
		if next.State != HalfOpen || next.Generation != m.Generation || next.ProbeSuccesses != 1 || len(next.Leases) != 0 {
			t.Fatalf("machine = %+v", next)
		}
		next, granted := next.requestProbe(2, now, 1, testLease)
		next = next.completeProbe(next.Generation, 2, Success, now, 2, testOpenDuration)
		if !granted || next.State != Closed || next.Generation != m.Generation+1 || next.ProbeSuccesses != 0 {
			t.Fatalf("granted = %v, machine = %+v", granted, next)
		}
	})

	t.Run("SM-6 an ignored probe removes the lease and changes no state", func(t *testing.T) {
		m, now := halfOpenMachine(t, 1)
		next := m.completeProbe(m.Generation, 1, Ignored, now, 1, testOpenDuration)
		if next.State != HalfOpen || next.Generation != m.Generation || next.ProbeSuccesses != 0 || len(next.Leases) != 0 {
			t.Fatalf("machine = %+v", next)
		}
	})

	t.Run("SM-5 a probe result removes only its own lease", func(t *testing.T) {
		m, now := halfOpenMachine(t, 3)
		m, _ = m.requestProbe(2, now, 3, testLease)
		m, _ = m.requestProbe(3, now, 3, testLease)
		next := m.completeProbe(m.Generation, 2, Success, now, 5, testOpenDuration)
		if len(next.Leases) != 2 || next.Leases[0].Token != 1 || next.Leases[1].Token != 3 {
			t.Fatalf("leases = %+v", next.Leases)
		}
	})
}

const (
	replayLimit     = 2
	replayThreshold = 2
)

type step struct {
	before, after Record
	now           time.Time
	stale         bool
}

func replay(events []byte) []step {
	var steps []step
	m := Record{}
	now := testStart
	token := uint64(0)
	for i := 0; i+2 < len(events); i += 3 {
		operation, argument, choice := events[i]%3, events[i+1], events[i+2]
		now = now.Add(time.Duration(argument%16) * time.Second)
		stale := choice&1 == 1
		generation := m.Generation
		if stale {
			generation += uint64(choice>>1) + 1
		}
		before := m
		switch operation {
		case 0:
			m = m.trip(generation, now, testOpenDuration)
		case 1:
			token++
			stale = false
			m, _ = m.requestProbe(token, now, replayLimit, testLease)
		default:
			m = m.completeProbe(generation, uint64(choice>>1)%(token+1), Result(argument%3), now, replayThreshold, testOpenDuration)
		}
		steps = append(steps, step{before: before, after: m, now: now, stale: stale})
	}
	return steps
}

func checkInvariants(t *testing.T, events []byte) {
	t.Helper()
	steps := replay(events)
	if !reflect.DeepEqual(steps, replay(events)) {
		t.Fatalf("SM-8 invariant 6: two runs of %v gave different results", events)
	}
	permitted := map[[2]State]bool{{Closed, Open}: true, {Open, HalfOpen}: true, {HalfOpen, Closed}: true, {HalfOpen, Open}: true}
	for i, s := range steps {
		stateChanged := s.before.State != s.after.State
		switch {
		case s.after.Generation < s.before.Generation:
			t.Fatalf("GEN-1 invariant 1: step %d: generation went from %d to %d", i, s.before.Generation, s.after.Generation)
		case stateChanged && !permitted[[2]State{s.before.State, s.after.State}]:
			t.Fatalf("SM-7 invariant 2: step %d: transition %v to %v", i, s.before.State, s.after.State)
		case stateChanged != (s.after.Generation == s.before.Generation+1), !stateChanged && s.after.Generation != s.before.Generation:
			t.Fatalf("GEN-1 invariant 3: step %d: state %v to %v, generation %d to %d", i, s.before.State, s.after.State, s.before.Generation, s.after.Generation)
		case s.stale && !reflect.DeepEqual(s.before, s.after):
			t.Fatalf("GEN-3 invariant 4: step %d: an event with a wrong generation changed %+v to %+v", i, s.before, s.after)
		case len(s.after.validLeases(s.now)) > replayLimit:
			t.Fatalf("HO-1 invariant 5: step %d: %d valid leases", i, len(s.after.validLeases(s.now)))
		}
	}
}

func FuzzMachine(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 1, 15, 0, 1, 15, 0, 2, 0, 2})
	f.Add([]byte{0, 0, 0, 1, 15, 0, 1, 15, 0, 2, 1, 2, 0, 0, 0})
	f.Add([]byte{0, 0, 1, 2, 0, 0, 1, 31, 0, 1, 31, 0, 2, 0, 2, 2, 0, 4})
	f.Fuzz(checkInvariants)
}

func TestMachineInvariants(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	visited := map[[2]State]bool{}
	for range 5000 {
		events := make([]byte, 3*random.IntN(40))
		for i := range events {
			events[i] = byte(random.UintN(256))
		}
		checkInvariants(t, events)
		for _, s := range replay(events) {
			if s.before.State != s.after.State {
				visited[[2]State{s.before.State, s.after.State}] = true
			}
		}
	}
	if len(visited) != 4 {
		t.Fatalf("the random sequences made the transitions %v, want all four", visited)
	}
}

func identified(instance, sequence uint64, failures int64) Report {
	return Report{
		Instance:      instance,
		Sequence:      sequence,
		Calls:         failures,
		Failures:      failures,
		Thresholds:    Thresholds{FailureThreshold: 100, MinimumCalls: 1 << 40},
		WindowSize:    10 * time.Second,
		WindowBuckets: 10,
		OpenDuration:  testOpenDuration,
	}
}

func windowFailures(r Record) int64 {
	var failures int64
	for _, b := range r.Buckets {
		failures += b.Failures
	}
	return failures
}

func TestReportIdentity(t *testing.T) {
	t.Run("FD-10 a report that the record applied before adds nothing", func(t *testing.T) {
		applied := Record{}.Report(testStart, identified(7, 1, 3))
		again := applied.Report(testStart.Add(time.Second), identified(7, 1, 3))
		assertUnchanged(t, applied, again)
	})

	t.Run("FD-10 a late report with a lower sequence adds nothing", func(t *testing.T) {
		applied := Record{}.Report(testStart, identified(7, 1, 3)).Report(testStart, identified(7, 2, 1))
		assertUnchanged(t, applied, applied.Report(testStart, identified(7, 1, 3)))
	})

	t.Run("FD-10 the next sequence of the same instance adds its counts", func(t *testing.T) {
		r := Record{}.Report(testStart, identified(7, 1, 3)).Report(testStart, identified(7, 2, 1))
		if got := windowFailures(r); got != 4 || len(r.Reporters) != 1 || r.Reporters[0].Sequence != 2 {
			t.Fatalf("failures = %d, reporters = %+v, want 4 failures and one reporter with sequence 2", got, r.Reporters)
		}
	})

	t.Run("FD-10 two instances with the same sequence add their counts", func(t *testing.T) {
		r := Record{}.Report(testStart, identified(7, 1, 3)).Report(testStart, identified(8, 1, 2))
		if got := windowFailures(r); got != 5 || len(r.Reporters) != 2 {
			t.Fatalf("failures = %d, reporters = %+v, want 5 failures and two reporters", got, r.Reporters)
		}
	})

	t.Run("FD-10 a report without a sequence is added each time and keeps the reporters", func(t *testing.T) {
		r := Record{}.Report(testStart, identified(0, 1, 1))
		r = r.Report(testStart, identified(0, 0, 3)).Report(testStart, identified(0, 0, 3))
		if got := windowFailures(r); got != 7 || len(r.Reporters) != 1 {
			t.Fatalf("failures = %d, reporters = %+v, want 7 failures and one reporter", got, r.Reporters)
		}
	})

	t.Run("FD-10 the record forgets a reporter when its counts left the window", func(t *testing.T) {
		r := Record{}.Report(testStart, identified(7, 1, 3))
		r = r.Report(testStart.Add(10*time.Second), identified(8, 1, 1))
		if len(r.Reporters) != 1 || r.Reporters[0].Instance != 8 {
			t.Fatalf("reporters = %+v, want only instance 8", r.Reporters)
		}
	})

	t.Run("FD-10 GEN-3 a transition removes the reporters", func(t *testing.T) {
		report := identified(7, 1, 3)
		report.Thresholds.FailureThreshold = 3
		if r := (Record{}).Report(testStart, report); r.State != Open || len(r.Reporters) != 0 {
			t.Fatalf("record = %+v, want open without reporters", r)
		}
	})
}
