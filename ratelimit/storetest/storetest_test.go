package storetest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/ratelimit"
)

func memory() *ratelimit.MemoryStore {
	return ratelimit.NewMemoryStore(faultvane.SystemClock())
}

func TestMemoryStoreConformance(t *testing.T) {
	t.Run("RL-21 QA-8 the in-memory store", func(t *testing.T) {
		Run(t, func(*testing.T) ratelimit.Store { return memory() })
	})
}

type changesTheRequest struct {
	*ratelimit.MemoryStore
	change func(*ratelimit.Acquire)
}

func (s changesTheRequest) Acquire(ctx context.Context, name string, request ratelimit.Acquire) (ratelimit.Grant, error) {
	s.change(&request)
	return s.MemoryStore.Acquire(ctx, name, request)
}

func changed(change func(*ratelimit.Acquire)) func() ratelimit.Store {
	return func() ratelimit.Store { return changesTheRequest{memory(), change} }
}

type startsEmpty struct {
	*ratelimit.MemoryStore
	seen *sync.Map
}

func (s startsEmpty) Acquire(ctx context.Context, name string, request ratelimit.Acquire) (ratelimit.Grant, error) {
	if _, found := s.seen.LoadOrStore(name, true); !found {
		_, _ = s.MemoryStore.Acquire(ctx, name, ratelimit.Acquire{Wanted: request.Burst, Rate: request.Rate, Burst: request.Burst})
	}
	return s.MemoryStore.Acquire(ctx, name, request)
}

type grantsAllWanted struct{ *ratelimit.MemoryStore }

func (s grantsAllWanted) Acquire(ctx context.Context, name string, request ratelimit.Acquire) (ratelimit.Grant, error) {
	grant, err := s.MemoryStore.Acquire(ctx, name, request)
	grant.Tokens = request.Wanted
	return grant, err
}

type losesOneToken struct{ *ratelimit.MemoryStore }

func (s losesOneToken) Acquire(ctx context.Context, name string, request ratelimit.Acquire) (ratelimit.Grant, error) {
	grant, err := s.MemoryStore.Acquire(ctx, name, request)
	if grant.Tokens > 0 {
		lost := request
		lost.Wanted, lost.Returned = 1, 0
		_, _ = s.MemoryStore.Acquire(ctx, name, lost)
	}
	return grant, err
}

type ignoresTheContext struct{ *ratelimit.MemoryStore }

func (s ignoresTheContext) Acquire(_ context.Context, name string, request ratelimit.Acquire) (ratelimit.Grant, error) {
	return s.MemoryStore.Acquire(context.Background(), name, request)
}

type sharesAllNames struct{ *ratelimit.MemoryStore }

func (s sharesAllNames) Acquire(ctx context.Context, _ string, request ratelimit.Acquire) (ratelimit.Grant, error) {
	return s.MemoryStore.Acquire(ctx, "shared", request)
}

type withoutStoreTime struct{ *ratelimit.MemoryStore }

func (s withoutStoreTime) Acquire(ctx context.Context, name string, request ratelimit.Acquire) (ratelimit.Grant, error) {
	grant, err := s.MemoryStore.Acquire(ctx, name, request)
	grant.StoreTime = time.Time{}
	return grant, err
}

type storeTimeGoesBack struct {
	*ratelimit.MemoryStore
	calls *atomic.Int64
}

func (s storeTimeGoesBack) Acquire(ctx context.Context, name string, request ratelimit.Acquire) (ratelimit.Grant, error) {
	grant, err := s.MemoryStore.Acquire(ctx, name, request)
	grant.StoreTime = grant.StoreTime.Add(-time.Duration(s.calls.Add(1)) * time.Hour)
	return grant, err
}

type notAtomic struct {
	mu     sync.Mutex
	tokens map[string]int64
}

func (s *notAtomic) Acquire(_ context.Context, name string, request ratelimit.Acquire) (ratelimit.Grant, error) {
	s.mu.Lock()
	tokens, found := s.tokens[name]
	s.mu.Unlock()
	if !found {
		tokens = request.Burst
	}
	time.Sleep(20 * time.Millisecond)
	granted := max(min(request.Wanted, tokens+request.Returned), 0)
	s.mu.Lock()
	s.tokens[name] = min(tokens+request.Returned, request.Burst) - granted
	s.mu.Unlock()
	return ratelimit.Grant{Tokens: granted, StoreTime: time.Now()}, nil
}

type unavailable struct{}

var errDown = errors.New("store down")

func (unavailable) Acquire(context.Context, string, ratelimit.Acquire) (ratelimit.Grant, error) {
	return ratelimit.Grant{}, errDown
}

func failedChecks(newStore func() ratelimit.Store) []string {
	var failed []string
	for _, c := range checks {
		if len(c.problems(newStore())) > 0 {
			failed = append(failed, c.name)
		}
	}
	return failed
}

func TestTheSuiteAcceptsTheReference(t *testing.T) {
	if failed := failedChecks(func() ratelimit.Store { return memory() }); len(failed) != 0 {
		t.Fatalf("RL-21 QA-8 the suite rejects the in-memory store: %v", failed)
	}
}

func TestTheSuiteRejectsBrokenStores(t *testing.T) {
	broken := map[string]func() ratelimit.Store{
		"starts with an empty bucket":   func() ratelimit.Store { return startsEmpty{memory(), &sync.Map{}} },
		"grants all wanted tokens":      func() ratelimit.Store { return grantsAllWanted{memory()} },
		"never refills":                 changed(func(r *ratelimit.Acquire) { r.Rate = 0 }),
		"refills at two times the rate": changed(func(r *ratelimit.Acquire) { r.Rate *= 2 }),
		"has a larger burst for a fast rate": changed(func(r *ratelimit.Acquire) {
			if r.Rate > fast {
				r.Burst *= 2
			}
		}),
		"ignores returned tokens":               changed(func(r *ratelimit.Acquire) { r.Returned = 0 }),
		"keeps returned tokens above the burst": changed(func(r *ratelimit.Acquire) { r.Burst += r.Returned }),
		"ignores the context":                   func() ratelimit.Store { return ignoresTheContext{memory()} },
		"shares all names":                      func() ratelimit.Store { return sharesAllNames{memory()} },
		"returns no store time":                 func() ratelimit.Store { return withoutStoreTime{memory()} },
		"returns a store time that goes back":   func() ratelimit.Store { return storeTimeGoesBack{memory(), &atomic.Int64{}} },
		"reads and writes in separate steps":    func() ratelimit.Store { return &notAtomic{tokens: map[string]int64{}} },
		"is not available":                      func() ratelimit.Store { return unavailable{} },
	}
	var mu sync.Mutex
	rejected := map[string]bool{}
	t.Run("broken stores", func(t *testing.T) {
		for name, newStore := range broken {
			t.Run("RL-21 QA-8 the suite rejects a store that "+name, func(t *testing.T) {
				t.Parallel()
				failed := failedChecks(newStore)
				if len(failed) == 0 {
					t.Fatal("no check failed")
				}
				mu.Lock()
				defer mu.Unlock()
				for _, name := range failed {
					rejected[name] = true
				}
			})
		}
	})
	t.Run("RL-21 QA-8 the concurrency check rejects a store that grants less than the bucket holds", func(t *testing.T) {
		concurrency := checks[len(checks)-1]
		if problems := concurrency.problems(losesOneToken{memory()}); len(problems) == 0 || !strings.HasPrefix(concurrency.name, "RS1 concurrent") {
			t.Fatalf("the check %q found no problem", concurrency.name)
		}
	})
	t.Run("RL-21 QA-8 each check rejects a broken store", func(t *testing.T) {
		for _, c := range checks {
			if !rejected[c.name] {
				t.Errorf("no broken store fails the check %q", c.name)
			}
		}
	})
}
