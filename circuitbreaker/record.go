package circuitbreaker

import (
	"slices"
	"time"
)

type Lease struct {
	Token     uint64
	ExpiresAt time.Time
}

type Bucket struct {
	Epoch     int64
	Calls     int64
	Failures  int64
	SlowCalls int64
}

type Record struct {
	State          State
	Generation     uint64
	TransitionAt   time.Time
	OpenDuration   time.Duration
	ProbeSuccesses int
	Leases         []Lease
	Buckets        []Bucket
	Fingerprint    string
}

func (r Record) transition(to State, now time.Time, openDuration time.Duration) Record {
	return Record{State: to, Generation: r.Generation + 1, TransitionAt: now, OpenDuration: openDuration, Fingerprint: r.Fingerprint}
}

func (r Record) openUntil() time.Time {
	return r.TransitionAt.Add(r.OpenDuration)
}

func (r Record) validLeases(now time.Time) []Lease {
	valid := make([]Lease, 0, len(r.Leases)+1)
	for _, l := range r.Leases {
		if l.ExpiresAt.After(now) {
			valid = append(valid, l)
		}
	}
	return valid
}

func (r Record) trip(generation uint64, now time.Time, openDuration time.Duration) Record {
	if r.State != Closed || r.Generation != generation {
		return r
	}
	return r.transition(Open, now, openDuration)
}

func (r Record) requestProbe(token uint64, now time.Time, limit int, leaseDuration time.Duration) (Record, bool) {
	switch r.State {
	case Closed:
		return r, false
	case Open:
		if now.Before(r.openUntil()) {
			return r, false
		}
		r = r.transition(HalfOpen, now, 0)
	}
	r.Leases = r.validLeases(now)
	if len(r.Leases) >= limit {
		return r, false
	}
	r.Leases = append(r.Leases, Lease{Token: token, ExpiresAt: now.Add(leaseDuration)})
	return r, true
}

func (r Record) completeProbe(generation, token uint64, result Result, now time.Time, successThreshold int, openDuration time.Duration) Record {
	if r.State != HalfOpen || r.Generation != generation {
		return r
	}
	r.Leases = r.validLeases(now)
	index := slices.IndexFunc(r.Leases, func(l Lease) bool { return l.Token == token })
	if index < 0 {
		return r
	}
	r.Leases = slices.Delete(r.Leases, index, index+1)
	switch result {
	case Failure:
		return r.transition(Open, now, openDuration)
	case Success:
		r.ProbeSuccesses++
		if r.ProbeSuccesses >= successThreshold {
			return r.transition(Closed, now, 0)
		}
	}
	return r
}

func (r Record) Snapshot(now time.Time) Snapshot {
	return Snapshot{State: r.State, Generation: r.Generation, TransitionAt: r.TransitionAt, OpenDuration: r.OpenDuration, StoreTime: now, Fingerprint: r.Fingerprint}
}

func (r Record) Apply(now time.Time, change func(Record, time.Time) Record) (Record, Snapshot) {
	next := change(r, now)
	snapshot := next.Snapshot(now)
	snapshot.Fingerprint = r.Fingerprint
	return next, snapshot
}

func (r Record) Report(now time.Time, report Report) Record {
	if r.State != Closed || r.Generation != report.Generation {
		return r
	}
	r.Fingerprint = report.Fingerprint
	epoch := now.UnixNano() / int64(report.WindowSize/time.Duration(report.WindowBuckets))
	oldest := epoch - int64(report.WindowBuckets) + 1
	buckets := make([]Bucket, 0, len(r.Buckets)+1)
	current := -1
	var calls, failures, slowCalls int64
	for _, b := range r.Buckets {
		if b.Epoch < oldest {
			continue
		}
		if b.Epoch == epoch {
			current = len(buckets)
		}
		buckets = append(buckets, b)
		calls, failures, slowCalls = calls+b.Calls, failures+b.Failures, slowCalls+b.SlowCalls
	}
	if current < 0 {
		current = len(buckets)
		buckets = append(buckets, Bucket{Epoch: epoch})
	}
	buckets[current].Calls += report.Calls
	buckets[current].Failures += report.Failures
	buckets[current].SlowCalls += report.SlowCalls
	r.Buckets = buckets
	if report.Thresholds.Exceeded(calls+report.Calls, failures+report.Failures, slowCalls+report.SlowCalls) {
		return r.trip(r.Generation, now, report.OpenDuration)
	}
	return r
}

func (r Record) AcquireProbe(now time.Time, request ProbeRequest) (Record, bool) {
	r, granted := r.requestProbe(request.Token, now, request.Limit, request.LeaseDuration)
	if r.State == HalfOpen {
		r.Fingerprint = request.Fingerprint
	}
	return r, granted
}

func (r Record) CompleteProbe(now time.Time, result ProbeResult) Record {
	if r.State == HalfOpen && r.Generation == result.Generation {
		r.Fingerprint = result.Fingerprint
	}
	return r.completeProbe(result.Generation, result.Token, result.Result, now, result.SuccessThreshold, result.OpenDuration)
}

func (r Record) Restore(snapshot Snapshot) Record {
	if snapshot.Generation <= r.Generation {
		return r
	}
	return Record{State: snapshot.State, Generation: snapshot.Generation, TransitionAt: snapshot.TransitionAt, OpenDuration: snapshot.OpenDuration, Fingerprint: r.Fingerprint}
}
