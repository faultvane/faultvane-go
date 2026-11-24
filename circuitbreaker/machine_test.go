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

func openMachine(t *testing.T) machine {
	t.Helper()
	m := machine{}.trip(0, testStart, testOpenDuration)
	if m.state != Open {
		t.Fatalf("state = %v, want open", m.state)
	}
	return m
}

func halfOpenMachine(t *testing.T, limit int) (machine, time.Time) {
	t.Helper()
	now := testStart.Add(testOpenDuration)
	m, granted := openMachine(t).requestProbe(1, now, limit, testLease)
	if m.state != HalfOpen || !granted {
		t.Fatalf("state = %v, granted = %v, want half_open, true", m.state, granted)
	}
	return m, now
}

func assertUnchanged(t *testing.T, before, after machine) {
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
		if m := (machine{}); m.state != Closed || m.generation != 0 {
			t.Fatalf("machine = %+v", m)
		}
	})

	t.Run("SM-2 T1 a trip opens a closed machine", func(t *testing.T) {
		m := machine{}.trip(0, testStart, testOpenDuration)
		want := machine{state: Open, generation: 1, transitionAt: testStart, openDuration: testOpenDuration}
		assertUnchanged(t, want, m)
		if until := m.openUntil(); !until.Equal(testStart.Add(testOpenDuration)) {
			t.Fatalf("openUntil = %v", until)
		}
	})

	t.Run("GEN-3 G5 a trip with an old generation changes nothing", func(t *testing.T) {
		m := machine{generation: 4}
		assertUnchanged(t, m, m.trip(3, testStart, testOpenDuration))
		assertUnchanged(t, m, m.trip(5, testStart, testOpenDuration))
	})

	t.Run("SM-7 a trip changes nothing when the machine is not closed", func(t *testing.T) {
		open := openMachine(t)
		assertUnchanged(t, open, open.trip(open.generation, testStart.Add(time.Second), time.Hour))
		halfOpen, now := halfOpenMachine(t, 1)
		assertUnchanged(t, halfOpen, halfOpen.trip(halfOpen.generation, now, time.Hour))
	})
}

func TestMachineRequestProbe(t *testing.T) {
	t.Run("SM-7 a closed machine grants no probe", func(t *testing.T) {
		m, granted := machine{}.requestProbe(1, testStart, 1, testLease)
		if granted {
			t.Fatal("granted = true")
		}
		assertUnchanged(t, machine{}, m)
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
		want := machine{state: HalfOpen, generation: 2, transitionAt: now, leases: []lease{{token: 1, expiresAt: now.Add(testLease)}}}
		assertUnchanged(t, want, m)
	})

	t.Run("SM-5 HO-4 a half-open machine grants no probe above the limit", func(t *testing.T) {
		m, now := halfOpenMachine(t, 1)
		next, granted := m.requestProbe(2, now.Add(time.Second), 1, testLease)
		if granted || next.generation != m.generation || len(next.leases) != 1 || next.leases[0].token != 1 {
			t.Fatalf("granted = %v, machine = %+v", granted, next)
		}
	})

	t.Run("SM-5 a half-open machine grants probes up to the limit", func(t *testing.T) {
		m, now := halfOpenMachine(t, 2)
		m, second := m.requestProbe(2, now, 2, testLease)
		_, third := m.requestProbe(3, now, 2, testLease)
		if !second || third || len(m.leases) != 2 {
			t.Fatalf("second = %v, third = %v, leases = %d, want true, false, 2", second, third, len(m.leases))
		}
	})

	t.Run("HO-3 a lease that ended gives its place to a new probe", func(t *testing.T) {
		m, now := halfOpenMachine(t, 1)
		_, early := m.requestProbe(2, now.Add(testLease-time.Nanosecond), 1, testLease)
		next, atEnd := m.requestProbe(2, now.Add(testLease), 1, testLease)
		if early || !atEnd || len(next.leases) != 1 || next.leases[0].token != 2 || next.generation != m.generation {
			t.Fatalf("early = %v, atEnd = %v, machine = %+v", early, atEnd, next)
		}
	})

	t.Run("SM-8 a probe request does not change the machine that it started from", func(t *testing.T) {
		m, now := halfOpenMachine(t, 2)
		_, _ = m.requestProbe(2, now, 2, testLease)
		if len(m.leases) != 1 {
			t.Fatalf("leases = %d, want 1", len(m.leases))
		}
	})
}

func TestMachineCompleteProbe(t *testing.T) {
	t.Run("SM-7 a probe result changes nothing when the machine is not half-open", func(t *testing.T) {
		open := openMachine(t)
		assertUnchanged(t, open, open.completeProbe(open.generation, 1, Success, testStart, 1, testOpenDuration))
		assertUnchanged(t, machine{}, machine{}.completeProbe(0, 1, Failure, testStart, 1, testOpenDuration))
	})

	t.Run("GEN-3 G4 a probe result with an old generation changes nothing", func(t *testing.T) {
		m, now := halfOpenMachine(t, 1)
		assertUnchanged(t, m, m.completeProbe(m.generation-1, 1, Success, now, 1, testOpenDuration))
		assertUnchanged(t, m, m.completeProbe(m.generation+1, 1, Failure, now, 1, testOpenDuration))
	})

	t.Run("HO-4 a probe result with an unknown token changes nothing", func(t *testing.T) {
		m, now := halfOpenMachine(t, 1)
		next := m.completeProbe(m.generation, 99, Failure, now, 1, testOpenDuration)
		if next.state != HalfOpen || next.generation != m.generation || len(next.leases) != 1 {
			t.Fatalf("machine = %+v", next)
		}
	})

	t.Run("HO-3 a probe result after the end of its lease changes nothing", func(t *testing.T) {
		m, now := halfOpenMachine(t, 1)
		next := m.completeProbe(m.generation, 1, Success, now.Add(testLease), 1, testOpenDuration)
		if next.state != HalfOpen || next.generation != m.generation || len(next.leases) != 0 || next.probeSuccesses != 0 {
			t.Fatalf("machine = %+v", next)
		}
	})

	t.Run("SM-6 T4 a failed probe opens the machine", func(t *testing.T) {
		m, now := halfOpenMachine(t, 1)
		at := now.Add(time.Second)
		next := m.completeProbe(m.generation, 1, Failure, at, 1, time.Minute)
		want := machine{state: Open, generation: 3, transitionAt: at, openDuration: time.Minute}
		assertUnchanged(t, want, next)
	})

	t.Run("SM-6 T3 a successful probe closes the machine at the success threshold", func(t *testing.T) {
		m, now := halfOpenMachine(t, 1)
		at := now.Add(time.Second)
		next := m.completeProbe(m.generation, 1, Success, at, 1, testOpenDuration)
		want := machine{state: Closed, generation: 3, transitionAt: at}
		assertUnchanged(t, want, next)
	})

	t.Run("SM-6 a successful probe below the success threshold keeps half-open", func(t *testing.T) {
		m, now := halfOpenMachine(t, 1)
		next := m.completeProbe(m.generation, 1, Success, now, 2, testOpenDuration)
		if next.state != HalfOpen || next.generation != m.generation || next.probeSuccesses != 1 || len(next.leases) != 0 {
			t.Fatalf("machine = %+v", next)
		}
		next, granted := next.requestProbe(2, now, 1, testLease)
		next = next.completeProbe(next.generation, 2, Success, now, 2, testOpenDuration)
		if !granted || next.state != Closed || next.generation != m.generation+1 || next.probeSuccesses != 0 {
			t.Fatalf("granted = %v, machine = %+v", granted, next)
		}
	})

	t.Run("SM-6 an ignored probe removes the lease and changes no state", func(t *testing.T) {
		m, now := halfOpenMachine(t, 1)
		next := m.completeProbe(m.generation, 1, Ignored, now, 1, testOpenDuration)
		if next.state != HalfOpen || next.generation != m.generation || next.probeSuccesses != 0 || len(next.leases) != 0 {
			t.Fatalf("machine = %+v", next)
		}
	})

	t.Run("SM-5 a probe result removes only its own lease", func(t *testing.T) {
		m, now := halfOpenMachine(t, 3)
		m, _ = m.requestProbe(2, now, 3, testLease)
		m, _ = m.requestProbe(3, now, 3, testLease)
		next := m.completeProbe(m.generation, 2, Success, now, 5, testOpenDuration)
		if len(next.leases) != 2 || next.leases[0].token != 1 || next.leases[1].token != 3 {
			t.Fatalf("leases = %+v", next.leases)
		}
	})
}

const (
	replayLimit     = 2
	replayThreshold = 2
)

type step struct {
	before, after machine
	now           time.Time
	stale         bool
}

func replay(events []byte) []step {
	var steps []step
	m := machine{}
	now := testStart
	token := uint64(0)
	for i := 0; i+2 < len(events); i += 3 {
		operation, argument, choice := events[i]%3, events[i+1], events[i+2]
		now = now.Add(time.Duration(argument%16) * time.Second)
		stale := choice&1 == 1
		generation := m.generation
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
		stateChanged := s.before.state != s.after.state
		switch {
		case s.after.generation < s.before.generation:
			t.Fatalf("GEN-1 invariant 1: step %d: generation went from %d to %d", i, s.before.generation, s.after.generation)
		case stateChanged && !permitted[[2]State{s.before.state, s.after.state}]:
			t.Fatalf("SM-7 invariant 2: step %d: transition %v to %v", i, s.before.state, s.after.state)
		case stateChanged != (s.after.generation == s.before.generation+1), !stateChanged && s.after.generation != s.before.generation:
			t.Fatalf("GEN-1 invariant 3: step %d: state %v to %v, generation %d to %d", i, s.before.state, s.after.state, s.before.generation, s.after.generation)
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
			if s.before.state != s.after.state {
				visited[[2]State{s.before.state, s.after.state}] = true
			}
		}
	}
	if len(visited) != 4 {
		t.Fatalf("the random sequences made the transitions %v, want all four", visited)
	}
}
