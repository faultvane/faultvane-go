package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/bulkhead"
	"github.com/faultvane/faultvane-go/circuitbreaker"
	"github.com/faultvane/faultvane-go/concurrencylimit"
	"github.com/faultvane/faultvane-go/loadshed"
	"github.com/faultvane/faultvane-go/ratelimit"
	"github.com/faultvane/faultvane-go/retry"
	mongostore "github.com/faultvane/faultvane-go/store/mongodb"
	redisstore "github.com/faultvane/faultvane-go/store/redis"
	sqlstore "github.com/faultvane/faultvane-go/store/sql"
	"github.com/faultvane/faultvane-go/timeout"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type setup struct {
	Namespace string `json:"namespace"`
	Store     string `json:"store"`
	Breaker   *struct {
		FailureThreshold int `json:"failureThreshold"`
		OpenMs           int `json:"openMs"`
		SyncMs           int `json:"syncMs"`
		ProbeTimeoutMs   int `json:"probeTimeoutMs"`
		LocalThreshold   int `json:"localThreshold"`
	} `json:"breaker"`
	Bulkhead *struct {
		Max     int `json:"max"`
		Waiting int `json:"waiting"`
	} `json:"bulkhead"`
	RateLimit *struct {
		Rate      float64 `json:"rate"`
		Burst     int     `json:"burst"`
		LocalRate float64 `json:"localRate"`
	} `json:"rateLimit"`
	ConcurrencyLimit *struct {
		Initial   int `json:"initial"`
		Min       int `json:"min"`
		Max       int `json:"max"`
		LatencyMs int `json:"latencyMs"`
	} `json:"concurrencyLimit"`
	LoadShed *struct {
		Max int `json:"max"`
	} `json:"loadShed"`
	Retry *struct {
		Attempts  int     `json:"attempts"`
		Budget    float64 `json:"budget"`
		GateState bool    `json:"gateState"`
	} `json:"retry"`
	TimeoutMs int `json:"timeoutMs"`
}

type load struct {
	Calls       int    `json:"calls"`
	Concurrency int    `json:"concurrency"`
	Priority    string `json:"priority"`
	Local       bool   `json:"local"`
	PauseMs     int    `json:"pauseMs"`
	DurationMs  int    `json:"durationMs"`
}

type result struct {
	OK         int            `json:"ok"`
	Failed     int            `json:"failed"`
	Timeouts   int            `json:"timeouts"`
	Rejected   map[string]int `json:"rejected"`
	DurationMs float64        `json:"durationMs"`
	P50Us      float64        `json:"p50Us"`
	P99Us      float64        `json:"p99Us"`
	P999Us     float64        `json:"p999Us"`
}

type status struct {
	Pod             string               `json:"pod"`
	State           string               `json:"state"`
	Mode            string               `json:"mode"`
	Breaker         circuitbreaker.Stats `json:"breaker"`
	StoreOperations int64                `json:"storeOperations"`
	Limit           int                  `json:"limit"`
	RateLimitMode   string               `json:"rateLimitMode"`
	RateLimit       ratelimit.Stats      `json:"rateLimit"`
}

type countingStore struct {
	circuitbreaker.Store
	operations *atomic.Int64
}

func (s countingStore) Load(ctx context.Context, name string) (circuitbreaker.Snapshot, error) {
	s.operations.Add(1)
	return s.Store.Load(ctx, name)
}

func (s countingStore) Report(ctx context.Context, name string, report circuitbreaker.Report) (circuitbreaker.Snapshot, error) {
	s.operations.Add(1)
	return s.Store.Report(ctx, name, report)
}

func (s countingStore) AcquireProbe(ctx context.Context, name string, request circuitbreaker.ProbeRequest) (circuitbreaker.ProbeGrant, error) {
	s.operations.Add(1)
	return s.Store.AcquireProbe(ctx, name, request)
}

func (s countingStore) CompleteProbe(ctx context.Context, name string, result circuitbreaker.ProbeResult) (circuitbreaker.Snapshot, error) {
	s.operations.Add(1)
	return s.Store.CompleteProbe(ctx, name, result)
}

type pod struct {
	name        string
	client      *http.Client
	mu          sync.Mutex
	pipeline    faultvane.Policy
	breaker     *circuitbreaker.Breaker
	limiter     *concurrencylimit.Limiter
	rateLimiter *ratelimit.Limiter
	operations  atomic.Int64
	closers     []func()
}

func duration(ms, fallback int) time.Duration {
	if ms == 0 {
		ms = fallback
	}
	return time.Duration(ms) * time.Millisecond
}

func (p *pod) redis() *redis.Client {
	client := redis.NewClient(&redis.Options{Addr: "redis:6379", DialTimeout: 200 * time.Millisecond, ReadTimeout: 200 * time.Millisecond, WriteTimeout: 200 * time.Millisecond, MaxRetries: -1})
	p.closers = append(p.closers, func() { _ = client.Close() })
	return client
}

func (p *pod) store(ctx context.Context, s setup) (circuitbreaker.Store, error) {
	switch s.Store {
	case "redis":
		return redisstore.New(p.redis(), redisstore.WithNamespace(s.Namespace))
	case "postgres", "mysql":
		driver, dsn, dialect := "pgx", "postgres://postgres:faultvane@postgres:5432/faultvane?sslmode=disable", sqlstore.PostgreSQL
		if s.Store == "mysql" {
			driver, dsn, dialect = "mysql", "root:faultvane@tcp(mysql:3306)/faultvane", sqlstore.MySQL
		}
		db, err := sql.Open(driver, dsn)
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(4)
		p.closers = append(p.closers, func() { _ = db.Close() })
		store, err := sqlstore.New(db, dialect, sqlstore.WithNamespace(s.Namespace))
		if err != nil {
			return nil, err
		}
		return store, store.Migrate(ctx)
	case "mongodb":
		client, err := mongo.Connect(options.Client().ApplyURI("mongodb://mongodb:27017").SetServerSelectionTimeout(300 * time.Millisecond).SetConnectTimeout(300 * time.Millisecond))
		if err != nil {
			return nil, err
		}
		p.closers = append(p.closers, func() { _ = client.Disconnect(context.Background()) })
		store, err := mongostore.New(client.Database("faultvane").Collection("circuit_breakers"), mongostore.WithNamespace(s.Namespace))
		if err != nil {
			return nil, err
		}
		return store, store.EnsureIndexes(ctx)
	}
	return nil, fmt.Errorf("unknown store %q", s.Store)
}

func (p *pod) configure(ctx context.Context, s setup) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.breaker != nil {
		_ = p.breaker.Close(ctx)
	}
	for _, closer := range slices.Backward(p.closers) {
		closer()
	}
	p.closers, p.breaker, p.limiter, p.rateLimiter, p.pipeline = nil, nil, nil, nil, nil
	p.operations.Store(0)
	var policies []faultvane.Policy
	if s.LoadShed != nil {
		shedder, err := loadshed.New(loadshed.WithMaxInFlight(s.LoadShed.Max))
		if err != nil {
			return err
		}
		policies = append(policies, shedder)
	}
	if r := s.RateLimit; r != nil {
		opts := []ratelimit.Option{ratelimit.WithRate(r.Rate), ratelimit.WithBurst(r.Burst)}
		if r.LocalRate > 0 {
			store, err := redisstore.NewRateLimitStore(p.redis(), redisstore.WithNamespace(s.Namespace))
			if err != nil {
				return err
			}
			opts = append(opts, ratelimit.WithStore(store), ratelimit.WithName("dependency"), ratelimit.WithLocalRate(r.LocalRate))
		}
		limiter, err := ratelimit.New(opts...)
		if err != nil {
			return err
		}
		p.closers = append(p.closers, func() { _ = limiter.Close(context.Background()) })
		p.rateLimiter = limiter
		policies = append(policies, limiter)
	}
	if c := s.ConcurrencyLimit; c != nil {
		limiter, err := concurrencylimit.New(concurrencylimit.WithMinLimit(c.Min), concurrencylimit.WithMaxLimit(c.Max), concurrencylimit.WithInitialLimit(c.Initial), concurrencylimit.WithLatencyThreshold(duration(c.LatencyMs, 50)))
		if err != nil {
			return err
		}
		p.limiter = limiter
		policies = append(policies, limiter)
	}
	if s.Bulkhead != nil {
		limit, err := bulkhead.New(bulkhead.WithMaxConcurrent(s.Bulkhead.Max), bulkhead.WithMaxWaiting(s.Bulkhead.Waiting))
		if err != nil {
			return err
		}
		policies = append(policies, limit)
	}
	if b := s.Breaker; b != nil {
		opts := []circuitbreaker.Option{
			circuitbreaker.WithLogger(slog.New(slog.NewTextHandler(os.Stderr, nil))),
			circuitbreaker.WithFailureThreshold(b.FailureThreshold),
			circuitbreaker.WithMinimumCalls(1 << 30),
			circuitbreaker.WithOpenDuration(duration(b.OpenMs, 60000)),
			circuitbreaker.WithProbeTimeout(duration(b.ProbeTimeoutMs, 30000)),
			circuitbreaker.WithProbeAttemptInterval(20 * time.Millisecond),
			circuitbreaker.WithFaultThreshold(2),
			circuitbreaker.WithStartTimeout(5 * time.Second),
		}
		if b.LocalThreshold > 0 {
			opts = append(opts, circuitbreaker.WithLocalFailureThreshold(b.LocalThreshold))
		}
		if s.Store != "" {
			store, err := p.store(ctx, s)
			if err != nil {
				return err
			}
			sync := duration(b.SyncMs, 200)
			opts = append(opts, circuitbreaker.WithStore(countingStore{Store: store, operations: &p.operations}),
				circuitbreaker.WithSyncInterval(sync), circuitbreaker.WithStoreTimeout(sync*4/5), circuitbreaker.WithEarlyReportInterval(sync/10))
		}
		breaker, err := circuitbreaker.New("dependency", opts...)
		if err != nil {
			return err
		}
		p.breaker = breaker
		policies = append(policies, breaker)
	}
	if r := s.Retry; r != nil {
		opts := []retry.Option{retry.WithMaxAttempts(r.Attempts), retry.WithBackoff(time.Millisecond, 5*time.Millisecond)}
		if r.Budget > 0 {
			opts = append(opts, retry.WithBudget(r.Budget))
		}
		if breaker := p.breaker; r.GateState && breaker != nil {
			opts = append(opts, retry.WithGate(func() bool { return breaker.State() == circuitbreaker.Closed }))
		}
		retrier, err := retry.New(opts...)
		if err != nil {
			return err
		}
		policies = append(policies, retrier)
	}
	if s.TimeoutMs > 0 {
		limited, err := timeout.New(duration(s.TimeoutMs, 0))
		if err != nil {
			return err
		}
		policies = append(policies, limited)
	}
	p.pipeline = faultvane.Combine(policies...)
	return nil
}

func (p *pod) call(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://dependency:8080/work?pod="+p.name, nil)
	if err != nil {
		return err
	}
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("dependency: status %d", response.StatusCode)
	}
	return nil
}

func classify(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, circuitbreaker.ErrCircuitOpen):
		return "open"
	case errors.Is(err, circuitbreaker.ErrProbeNotPermitted):
		return "probe"
	case errors.Is(err, bulkhead.ErrFull):
		return "bulkhead"
	case errors.Is(err, concurrencylimit.ErrLimitExceeded):
		return "concurrency"
	case errors.Is(err, ratelimit.ErrRateLimited):
		return "rate"
	case errors.Is(err, loadshed.ErrShed):
		return "shed"
	case errors.Is(err, timeout.ErrTimeout):
		return "timeout"
	}
	return "failed"
}

func (p *pod) run(l load) result {
	p.mu.Lock()
	pipeline := p.pipeline
	p.mu.Unlock()
	ctx := context.Background()
	switch l.Priority {
	case "low":
		ctx = loadshed.WithPriority(ctx, loadshed.Low)
	case "critical":
		ctx = loadshed.WithPriority(ctx, loadshed.Critical)
	}
	call := p.call
	if l.Local {
		call = func(context.Context) error { return nil }
	}
	workers := max(l.Concurrency, 1)
	outcomes := make([]map[string]int, workers)
	samples := make([][]time.Duration, workers)
	var next atomic.Int64
	var group sync.WaitGroup
	started := time.Now()
	more := func() bool { return next.Add(1) <= int64(l.Calls) }
	if l.DurationMs > 0 {
		more = func() bool { return time.Since(started) < time.Duration(l.DurationMs)*time.Millisecond }
	}
	for w := range workers {
		outcomes[w] = map[string]int{}
		group.Add(1)
		go func() {
			defer group.Done()
			for more() {
				begin := time.Now()
				err := pipeline.Do(ctx, call)
				samples[w] = append(samples[w], time.Since(begin))
				outcomes[w][classify(err)]++
				if l.PauseMs > 0 {
					time.Sleep(time.Duration(l.PauseMs) * time.Millisecond)
				}
			}
		}()
	}
	group.Wait()
	r := result{Rejected: map[string]int{}, DurationMs: float64(time.Since(started).Microseconds()) / 1000}
	var all []time.Duration
	for w := range workers {
		all = append(all, samples[w]...)
		for kind, count := range outcomes[w] {
			switch kind {
			case "ok":
				r.OK += count
			case "failed":
				r.Failed += count
			case "timeout":
				r.Timeouts += count
			default:
				r.Rejected[kind] += count
			}
		}
	}
	if len(all) > 0 {
		slices.Sort(all)
		at := func(q float64) float64 { return float64(all[int(float64(len(all)-1)*q)].Nanoseconds()) / 1000 }
		r.P50Us, r.P99Us, r.P999Us = at(0.5), at(0.99), at(0.999)
	}
	return r
}

func (p *pod) status() status {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := status{Pod: p.name, StoreOperations: p.operations.Load()}
	if p.breaker != nil {
		s.Breaker = p.breaker.Stats()
		s.State, s.Mode = s.Breaker.State.String(), s.Breaker.Mode.String()
	}
	if p.limiter != nil {
		s.Limit = p.limiter.Limit()
	}
	if p.rateLimiter != nil {
		s.RateLimit = p.rateLimiter.Stats()
		s.RateLimitMode = s.RateLimit.Mode.String()
	}
	return s
}

func main() {
	name, _ := os.Hostname()
	p := &pod{name: name, client: &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 256}}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /setup", func(w http.ResponseWriter, r *http.Request) {
		var s setup
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := p.configure(r.Context(), s); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("POST /load", func(w http.ResponseWriter, r *http.Request) {
		var l load
		if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(p.run(l))
	})
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(p.status())
	})
	mux.HandleFunc("GET /ready", func(http.ResponseWriter, *http.Request) {})
	log.Fatal(http.ListenAndServe(":8080", mux))
}
