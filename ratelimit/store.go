package ratelimit

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/faultvane/faultvane-go"
)

type Store interface {
	Acquire(ctx context.Context, name string, request Acquire) (Grant, error)
}

type Acquire struct {
	Wanted   int64
	Returned int64
	Rate     float64
	Burst    int64
	StateTTL time.Duration
}

type Grant struct {
	Tokens    int64
	StoreTime time.Time
}

type Bucket struct {
	Tokens float64
	At     time.Time
}

func (b Bucket) Acquire(now time.Time, request Acquire) (Bucket, Grant) {
	if now.Before(b.At) {
		now = b.At
	}
	tokens := min(float64(request.Burst), b.Tokens+now.Sub(b.At).Seconds()*request.Rate+float64(request.Returned))
	granted := max(math.Floor(min(float64(request.Wanted), tokens)), 0)
	return Bucket{Tokens: tokens - granted, At: now}, Grant{Tokens: int64(granted), StoreTime: now}
}

type MemoryStore struct {
	clock   faultvane.Clock
	mu      sync.Mutex
	buckets map[string]Bucket
}

func NewMemoryStore(clock faultvane.Clock) *MemoryStore {
	return &MemoryStore{clock: clock, buckets: map[string]Bucket{}}
}

func (s *MemoryStore) Acquire(ctx context.Context, name string, request Acquire) (Grant, error) {
	if err := ctx.Err(); err != nil {
		return Grant{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()
	bucket, found := s.buckets[name]
	if !found {
		bucket = Bucket{Tokens: float64(request.Burst), At: now}
	}
	bucket, grant := bucket.Acquire(now, request)
	s.buckets[name] = bucket
	return grant, nil
}
