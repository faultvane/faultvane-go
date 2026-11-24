package circuitbreaker

import (
	"slices"
	"time"
)

type lease struct {
	token     uint64
	expiresAt time.Time
}

type machine struct {
	state          State
	generation     uint64
	transitionAt   time.Time
	openDuration   time.Duration
	probeSuccesses int
	leases         []lease
}

func (m machine) transition(to State, now time.Time, openDuration time.Duration) machine {
	return machine{state: to, generation: m.generation + 1, transitionAt: now, openDuration: openDuration}
}

func (m machine) openUntil() time.Time {
	return m.transitionAt.Add(m.openDuration)
}

func (m machine) validLeases(now time.Time) []lease {
	valid := make([]lease, 0, len(m.leases)+1)
	for _, l := range m.leases {
		if l.expiresAt.After(now) {
			valid = append(valid, l)
		}
	}
	return valid
}

func (m machine) trip(generation uint64, now time.Time, openDuration time.Duration) machine {
	if m.state != Closed || m.generation != generation {
		return m
	}
	return m.transition(Open, now, openDuration)
}

func (m machine) requestProbe(token uint64, now time.Time, limit int, leaseDuration time.Duration) (machine, bool) {
	switch m.state {
	case Closed:
		return m, false
	case Open:
		if now.Before(m.openUntil()) {
			return m, false
		}
		m = m.transition(HalfOpen, now, 0)
	}
	m.leases = m.validLeases(now)
	if len(m.leases) >= limit {
		return m, false
	}
	m.leases = append(m.leases, lease{token: token, expiresAt: now.Add(leaseDuration)})
	return m, true
}

func (m machine) completeProbe(generation, token uint64, result Result, now time.Time, successThreshold int, openDuration time.Duration) machine {
	if m.state != HalfOpen || m.generation != generation {
		return m
	}
	m.leases = m.validLeases(now)
	index := slices.IndexFunc(m.leases, func(l lease) bool { return l.token == token })
	if index < 0 {
		return m
	}
	m.leases = slices.Delete(m.leases, index, index+1)
	switch result {
	case Failure:
		return m.transition(Open, now, openDuration)
	case Success:
		m.probeSuccesses++
		if m.probeSuccesses >= successThreshold {
			return m.transition(Closed, now, 0)
		}
	}
	return m
}
