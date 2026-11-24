package ratelimit

import (
	"context"
	"math"
	"sync"
	"time"
)

type Mode uint8

const (
	Distributed Mode = iota
	Local
)

func (m Mode) String() string {
	if m == Distributed {
		return "distributed"
	}
	return "local"
}

const maxWhole = 1 << 62

func whole(tokens float64) int64 {
	return int64(min(tokens, maxWhole))
}

type shared struct {
	cfg         config
	request     Acquire
	localBurst  float64
	stop        chan struct{}
	done        chan struct{}
	closeOnce   sync.Once
	mode        Mode
	faults      int
	storeErrors int64
	seen        int64
	reported    Mode
}

func (l *Limiter) start(cfg config) {
	s := &shared{cfg: cfg, mode: Local, stop: make(chan struct{}), done: make(chan struct{})}
	burst := float64(cfg.burst)
	s.request = Acquire{
		Rate:     cfg.rate,
		Burst:    int64(cfg.burst),
		StateTTL: time.Minute + time.Duration(min(burst/cfg.rate*float64(time.Second), maxWhole)),
	}
	s.localBurst = math.Ceil(burst * (cfg.localRate / cfg.rate))
	l.shared = s
	l.rate, l.burst, l.tokens = cfg.localRate, s.localBurst, s.localBurst
	l.synchronize(whole(math.Ceil(cfg.rate * cfg.syncInterval.Seconds())))
	if cfg.manualSync {
		close(s.done)
		return
	}
	go l.run()
}

func (l *Limiter) demand(minimum int64) Acquire {
	s := l.shared
	l.mu.Lock()
	defer l.mu.Unlock()
	calls := l.permitted + l.rejected
	batch := min(s.request.Burst, max(minimum, 2*(calls-s.seen)))
	s.seen = calls
	held := int64(0)
	if s.mode == Distributed {
		held = whole(l.tokens)
	}
	request := s.request
	request.Wanted = max(batch-held, 0)
	request.Returned = max(held-batch, 0)
	l.tokens -= float64(request.Returned)
	return request
}

func (l *Limiter) enterLocal(now time.Time) {
	s := l.shared
	if s.mode == Local {
		return
	}
	s.mode = Local
	l.rate, l.burst = s.cfg.localRate, s.localBurst
	l.since, l.tokens = now, min(l.tokens, s.localBurst)
}

func (l *Limiter) synchronize(minimum int64) {
	s := l.shared
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.storeTimeout)
	defer cancel()
	grant, err := s.cfg.store.Acquire(ctx, s.cfg.name, l.demand(minimum))
	now := l.clock.Now()
	defer l.report(err)
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		s.storeErrors++
		s.faults++
		if s.faults >= s.cfg.faultThreshold {
			l.enterLocal(now)
		}
		return
	}
	s.faults = 0
	if s.mode == Local {
		s.mode = Distributed
		l.rate, l.burst, l.tokens = 0, float64(s.request.Burst), 0
	}
	l.tokens += float64(grant.Tokens)
}

func (l *Limiter) run() {
	s := l.shared
	defer close(s.done)
	ticker := time.NewTicker(s.cfg.syncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			l.synchronize(1)
		}
	}
}

func (l *Limiter) Close(ctx context.Context) error {
	s := l.shared
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() { close(s.stop) })
	select {
	case <-s.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	now := l.clock.Now()
	defer l.report(nil)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.enterLocal(now)
	return nil
}
