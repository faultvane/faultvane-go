package circuitbreaker

import (
	"context"
	"sync"
	"time"

	"github.com/faultvane/faultvane-go"
)

type Store interface {
	Load(ctx context.Context, name string) (Snapshot, error)
	Report(ctx context.Context, name string, report Report) (Snapshot, error)
	AcquireProbe(ctx context.Context, name string, request ProbeRequest) (ProbeGrant, error)
	CompleteProbe(ctx context.Context, name string, result ProbeResult) (Snapshot, error)
	Restore(ctx context.Context, name string, snapshot Snapshot) (Snapshot, error)
}

type Snapshot struct {
	State        State
	Generation   uint64
	TransitionAt time.Time
	OpenDuration time.Duration
	StoreTime    time.Time
	Fingerprint  string
}

type Report struct {
	Generation    uint64
	Calls         int64
	Failures      int64
	SlowCalls     int64
	Thresholds    Thresholds
	WindowSize    time.Duration
	WindowBuckets int
	OpenDuration  time.Duration
	StateTTL      time.Duration
	Fingerprint   string
}

type ProbeRequest struct {
	Token         uint64
	Limit         int
	LeaseDuration time.Duration
	StateTTL      time.Duration
	Fingerprint   string
}

type ProbeGrant struct {
	Snapshot Snapshot
	Granted  bool
}

type ProbeResult struct {
	Generation       uint64
	Token            uint64
	Result           Result
	SuccessThreshold int
	OpenDuration     time.Duration
	StateTTL         time.Duration
	Fingerprint      string
}

type MemoryStore struct {
	clock   faultvane.Clock
	mu      sync.Mutex
	records map[string]Record
}

func NewMemoryStore(clock faultvane.Clock) *MemoryStore {
	return &MemoryStore{clock: clock, records: map[string]Record{}}
}

func (s *MemoryStore) update(ctx context.Context, name string, change func(Record, time.Time) Record) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, snapshot := s.records[name].Apply(s.clock.Now(), change)
	s.records[name] = record
	return snapshot, nil
}

func (s *MemoryStore) Load(ctx context.Context, name string) (Snapshot, error) {
	return s.update(ctx, name, func(r Record, _ time.Time) Record { return r })
}

func (s *MemoryStore) Report(ctx context.Context, name string, report Report) (Snapshot, error) {
	return s.update(ctx, name, func(r Record, now time.Time) Record { return r.Report(now, report) })
}

func (s *MemoryStore) AcquireProbe(ctx context.Context, name string, request ProbeRequest) (ProbeGrant, error) {
	granted := false
	snapshot, err := s.update(ctx, name, func(r Record, now time.Time) Record {
		r, granted = r.AcquireProbe(now, request)
		return r
	})
	return ProbeGrant{Snapshot: snapshot, Granted: granted}, err
}

func (s *MemoryStore) CompleteProbe(ctx context.Context, name string, result ProbeResult) (Snapshot, error) {
	return s.update(ctx, name, func(r Record, now time.Time) Record { return r.CompleteProbe(now, result) })
}

func (s *MemoryStore) Restore(ctx context.Context, name string, snapshot Snapshot) (Snapshot, error) {
	return s.update(ctx, name, func(r Record, _ time.Time) Record { return r.Restore(snapshot) })
}

func (s *MemoryStore) Delete(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, name)
}
