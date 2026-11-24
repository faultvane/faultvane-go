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

type sharedBucket struct {
	tokens float64
	at     time.Time
}

type MemoryStore struct {
	clock   faultvane.Clock
	mu      sync.Mutex
	buckets map[string]sharedBucket
}

func NewMemoryStore(clock faultvane.Clock) *MemoryStore {
	return &MemoryStore{clock: clock, buckets: map[string]sharedBucket{}}
}

func (s *MemoryStore) Acquire(ctx context.Context, name string, request Acquire) (Grant, error) {
	if err := ctx.Err(); err != nil {
		return Grant{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()
	burst := float64(request.Burst)
	bucket, found := s.buckets[name]
	if !found {
		bucket = sharedBucket{tokens: burst, at: now}
	}
	if now.Before(bucket.at) {
		now = bucket.at
	}
	tokens := min(burst, bucket.tokens+now.Sub(bucket.at).Seconds()*request.Rate+float64(request.Returned))
	granted := max(math.Floor(min(float64(request.Wanted), tokens)), 0)
	s.buckets[name] = sharedBucket{tokens: tokens - granted, at: now}
	return Grant{Tokens: int64(granted), StoreTime: now}, nil
}
