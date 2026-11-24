package redisstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/circuitbreaker"
	"github.com/faultvane/faultvane-go/circuitbreaker/storetest"
	"github.com/redis/go-redis/v9"
)

var (
	namespaceCounter atomic.Uint64
	errDependency    = errors.New("dependency failed")
	quiet            = slog.New(slog.DiscardHandler)
)

func address(variable, fallback string) string {
	if value := os.Getenv(variable); value != "" {
		return value
	}
	return fallback
}

func standalone() string { return address("FAULTVANE_REDIS_ADDR", "127.0.0.1:6399") }

func cluster() string { return address("FAULTVANE_REDIS_CLUSTER_ADDR", "127.0.0.1:6398") }

func newClient(t *testing.T, addr string) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: addr, DialTimeout: time.Second, MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func reachable(t *testing.T, addr string) *redis.Client {
	t.Helper()
	client := newClient(t, addr)
	eventually(t, "Redis at "+addr+" answers", func() bool {
		return client.Set(context.Background(), "faultvane-test-ready", 1, time.Second).Err() == nil
	})
	return client
}

func newStore(t *testing.T, client redis.Scripter) *Store {
	t.Helper()
	namespace := fmt.Sprintf("test-%d-%d", time.Now().UnixNano(), namespaceCounter.Add(1))
	store, err := New(client, WithNamespace(namespace))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func report(generation uint64, failures int64, ttl time.Duration) circuitbreaker.Report {
	return circuitbreaker.Report{
		Generation:    generation,
		Calls:         failures,
		Failures:      failures,
		Thresholds:    circuitbreaker.Thresholds{FailureThreshold: 5, MinimumCalls: 1000, FailureRate: 1},
		WindowSize:    time.Minute,
		WindowBuckets: 6,
		OpenDuration:  time.Hour,
		StateTTL:      ttl,
	}
}

func TestConformance(t *testing.T) {
	t.Run("QA-8 DS-6 GEN-2 standalone Redis", func(t *testing.T) {
		client := reachable(t, standalone())
		storetest.Run(t, func(t *testing.T) circuitbreaker.Store { return newStore(t, client) })
	})

	t.Run("QA-8 KEY-2 Redis with cluster mode: all keys of one breaker are in one slot", func(t *testing.T) {
		client := reachable(t, cluster())
		storetest.Run(t, func(t *testing.T) circuitbreaker.Store { return newStore(t, client) })
	})
}

func TestNew(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: standalone()})
	defer client.Close()
	invalid := map[string][]Option{
		"SEC-1 CFG-3 no namespace":                {},
		"CFG-3 namespace with a colon":            {WithNamespace("a:b")},
		"CFG-3 namespace with a brace":            {WithNamespace("{a}")},
		"CFG-3 namespace with 65 characters":      {WithNamespace(string(make([]byte, 65)))},
		"CFG-3 empty key prefix":                  {WithNamespace("a"), WithKeyPrefix("")},
		"CFG-3 key prefix with a space":           {WithNamespace("a"), WithKeyPrefix("a b")},
		"CFG-3 namespace outside the ASCII range": {WithNamespace("café")},
	}
	for name, opts := range invalid {
		t.Run(name, func(t *testing.T) {
			if store, err := New(client, opts...); store != nil || !errors.Is(err, faultvane.ErrInvalidConfig) {
				t.Fatalf("New() = %v, %v, want nil and ErrInvalidConfig", store, err)
			}
		})
	}

	t.Run("CFG-3 nil client", func(t *testing.T) {
		if store, err := New(nil, WithNamespace("a")); store != nil || !errors.Is(err, faultvane.ErrInvalidConfig) {
			t.Fatalf("New() = %v, %v", store, err)
		}
	})

	t.Run("KEY-1 KEY-2 the key names are deterministic and share one hash tag", func(t *testing.T) {
		store, err := New(client, WithNamespace("production"))
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			"faultvane:{production:payment-service}:cb:state",
			"faultvane:{production:payment-service}:cb:window",
			"faultvane:{production:payment-service}:cb:probes",
		}
		if got := store.keys("payment-service"); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("keys = %v, want %v", got, want)
		}
		custom, err := New(client, WithNamespace("azAZ09._-"), WithKeyPrefix("app"))
		if err != nil || custom.keys("x")[0] != "app:{azAZ09._-:x}:cb:state" {
			t.Fatalf("keys = %v, err = %v", custom.keys("x"), err)
		}
	})
}

func TestData(t *testing.T) {
	client := reachable(t, standalone())
	ctx := context.Background()

	t.Run("KEY-1 DS-2 the state, the window, and the leases are in the documented keys", func(t *testing.T) {
		store := newStore(t, client)
		keys := store.keys("a")
		if _, err := store.Report(ctx, "a", report(0, 2, time.Hour)); err != nil {
			t.Fatal(err)
		}
		state := client.HGetAll(ctx, keys[0]).Val()
		if state["v"] != "1" || state["st"] != "0" || state["gen"] != "0" || state["ps"] != "0" {
			t.Fatalf("state hash = %v", state)
		}
		window := client.HGetAll(ctx, keys[1]).Val()
		if len(window) != 3 {
			t.Fatalf("window hash = %v, want 3 fields of one bucket", window)
		}
		for field, value := range window {
			want := "2"
			if field[len(field)-1] == 's' {
				want = "0"
			}
			if value != want {
				t.Fatalf("window field %s = %s, want %s", field, value, want)
			}
		}
		if _, err := store.Report(ctx, "a", report(0, 3, time.Hour)); err != nil {
			t.Fatal(err)
		}
		if state := client.HGetAll(ctx, keys[0]).Val(); state["st"] != "1" || state["gen"] != "1" || client.Exists(ctx, keys[1]).Val() != 0 {
			t.Fatalf("state hash after the transition = %v", state)
		}
		lost := circuitbreaker.Snapshot{State: circuitbreaker.Open, Generation: 5, TransitionAt: time.Now().Add(-time.Hour), OpenDuration: time.Second}
		if _, err := store.Restore(ctx, "a", lost); err != nil {
			t.Fatal(err)
		}
		grant, err := store.AcquireProbe(ctx, "a", circuitbreaker.ProbeRequest{Token: 18446744073709551615, Limit: 1, LeaseDuration: time.Minute, StateTTL: time.Hour})
		if err != nil || !grant.Granted {
			t.Fatalf("AcquireProbe() = %+v, %v", grant, err)
		}
		if members := client.ZRange(ctx, keys[2], 0, -1).Val(); len(members) != 1 || members[0] != "18446744073709551615" {
			t.Fatalf("leases = %v", members)
		}
	})

	t.Run("GEN-1 DS-4 large generations and times keep their exact value", func(t *testing.T) {
		store := newStore(t, client)
		at := time.UnixMicro(time.Now().UnixMicro())
		lost := circuitbreaker.Snapshot{State: circuitbreaker.Open, Generation: 1 << 52, TransitionAt: at, OpenDuration: 36 * time.Hour}
		if _, err := store.Restore(ctx, "a", lost); err != nil {
			t.Fatal(err)
		}
		got, err := store.Load(ctx, "a")
		if err != nil || got.State != circuitbreaker.Open || got.Generation != 1<<52 || !got.TransitionAt.Equal(at) || got.OpenDuration != 36*time.Hour {
			t.Fatalf("Load() = %+v, %v, want %+v", got, err, lost)
		}
		if skew := time.Since(got.StoreTime).Abs(); skew > time.Hour {
			t.Fatalf("store time %v is %v from the local time", got.StoreTime, skew)
		}
	})

	t.Run("TTL-1 TTL-2 each write sets the TTL on all keys and unused keys expire", func(t *testing.T) {
		store := newStore(t, client)
		keys := store.keys("a")
		if _, err := store.Report(ctx, "a", report(0, 1, time.Hour)); err != nil {
			t.Fatal(err)
		}
		for _, key := range keys[:2] {
			if ttl := client.PTTL(ctx, key).Val(); ttl <= 59*time.Minute || ttl > time.Hour {
				t.Fatalf("TTL of %s = %v, want about 1h", key, ttl)
			}
		}
		if _, err := store.Report(ctx, "a", report(0, 1, 2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if ttl := client.PTTL(ctx, keys[0]).Val(); ttl <= time.Hour {
			t.Fatalf("TTL after the second write = %v, want about 2h", ttl)
		}
		if _, err := store.Report(ctx, "a", report(0, 3, 30*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the keys expire", func() bool { return client.Exists(ctx, keys...).Val() == 0 })
		if snapshot, err := store.Load(ctx, "a"); err != nil || snapshot.State != circuitbreaker.Closed || snapshot.Generation != 0 {
			t.Fatalf("Load() after the expiry = %+v, %v", snapshot, err)
		}
	})

	t.Run("TTL-2 a lease keeps its TTL with the state", func(t *testing.T) {
		store := newStore(t, client)
		keys := store.keys("a")
		lost := circuitbreaker.Snapshot{State: circuitbreaker.HalfOpen, Generation: 2, TransitionAt: time.Now()}
		if _, err := store.Restore(ctx, "a", lost); err != nil {
			t.Fatal(err)
		}
		if ttl := client.PTTL(ctx, keys[0]).Val(); ttl <= 59*time.Minute || ttl > time.Hour {
			t.Fatalf("TTL after restore = %v, want about 1h", ttl)
		}
		if _, err := store.AcquireProbe(ctx, "a", circuitbreaker.ProbeRequest{Token: 1, Limit: 1, LeaseDuration: time.Minute, StateTTL: 3 * time.Hour}); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{keys[0], keys[2]} {
			if ttl := client.PTTL(ctx, key).Val(); ttl <= 2*time.Hour {
				t.Fatalf("TTL of %s = %v, want about 3h", key, ttl)
			}
		}
		if _, err := store.CompleteProbe(ctx, "a", circuitbreaker.ProbeResult{Generation: 2, Token: 1, Result: circuitbreaker.Ignored, SuccessThreshold: 1, StateTTL: 4 * time.Hour}); err != nil {
			t.Fatal(err)
		}
		if ttl := client.PTTL(ctx, keys[0]).Val(); ttl <= 3*time.Hour {
			t.Fatalf("TTL after the probe result = %v, want about 4h", ttl)
		}
	})

	t.Run("RF-1 the store sends a script again after Redis lost its script cache", func(t *testing.T) {
		store := newStore(t, client)
		if _, err := store.Load(ctx, "a"); err != nil {
			t.Fatal(err)
		}
		if err := client.ScriptFlush(ctx).Err(); err != nil {
			t.Fatal(err)
		}
		if snapshot, err := store.Report(ctx, "a", report(0, 5, time.Hour)); err != nil || snapshot.State != circuitbreaker.Open {
			t.Fatalf("Report() = %+v, %v", snapshot, err)
		}
	})
}

func TestFaults(t *testing.T) {
	t.Run("RF-1 ERR-3 each operation on a Redis that does not answer returns ErrStoreUnavailable", func(t *testing.T) {
		client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
		defer client.Close()
		store := newStore(t, client)
		ctx := context.Background()
		_, loadErr := store.Load(ctx, "a")
		_, reportErr := store.Report(ctx, "a", report(0, 1, time.Hour))
		grant, acquireErr := store.AcquireProbe(ctx, "a", circuitbreaker.ProbeRequest{Token: 1, Limit: 1})
		_, completeErr := store.CompleteProbe(ctx, "a", circuitbreaker.ProbeResult{Token: 1})
		_, restoreErr := store.Restore(ctx, "a", circuitbreaker.Snapshot{Generation: 1})
		for operation, err := range map[string]error{"load": loadErr, "report": reportErr, "acquire probe": acquireErr, "complete probe": completeErr, "restore": restoreErr} {
			if !errors.Is(err, faultvane.ErrStoreUnavailable) || err.Error()[:len("redisstore: "+operation)] != "redisstore: "+operation {
				t.Errorf("%s: error = %v", operation, err)
			}
		}
		if grant.Granted {
			t.Error("a failed probe request is granted")
		}
	})

	t.Run("RF-2 API-3 an operation stops when its context ends", func(t *testing.T) {
		store := newStore(t, reachable(t, standalone()))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := store.Load(ctx, "a"); !errors.Is(err, context.Canceled) || !errors.Is(err, faultvane.ErrStoreUnavailable) {
			t.Fatalf("Load() error = %v, want context.Canceled and ErrStoreUnavailable", err)
		}
	})
}

func TestParse(t *testing.T) {
	t.Run("DS-5 a reply of integers and strings gives a snapshot", func(t *testing.T) {
		snapshot, granted, err := parse([]any{int64(2), "7", "1700000000000001", "30000000", "1700000000000002", int64(1)})
		want := circuitbreaker.Snapshot{
			State: circuitbreaker.HalfOpen, Generation: 7,
			TransitionAt: time.UnixMicro(1700000000000001), OpenDuration: 30 * time.Second, StoreTime: time.UnixMicro(1700000000000002),
		}
		if err != nil || !granted || snapshot != want {
			t.Fatalf("parse() = %+v, %v, %v, want %+v", snapshot, granted, err, want)
		}
		if _, granted, _ := parse([]any{int64(0), "0", "0", "0", "0", int64(0)}); granted {
			t.Fatal("granted = true for 0")
		}
	})

	for name, raw := range map[string]any{
		"RF-1 a reply that is not a list":          "OK",
		"RF-1 a reply with 5 values":               []any{int64(0), "0", "0", "0", "0"},
		"RF-1 a reply with 7 values":               []any{int64(0), "0", "0", "0", "0", int64(0), int64(0)},
		"RF-1 a reply with a value of a bad type":  []any{int64(0), 1.5, "0", "0", "0", int64(0)},
		"RF-1 a reply with text that is no number": []any{int64(0), "x", "0", "0", "0", int64(0)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parse(raw); err == nil {
				t.Fatal("parse() returned no error")
			}
		})
	}

	t.Run("DS-4 durations change to the units of Redis without a loss to zero", func(t *testing.T) {
		cases := []struct {
			d              time.Duration
			micros, millis int64
		}{{0, 0, 1}, {time.Nanosecond, 1, 1}, {999 * time.Nanosecond, 1, 1}, {time.Microsecond, 1, 1}, {1500 * time.Microsecond, 1500, 1}, {time.Hour, 3600000000, 3600000}, {-time.Second, -1000000, 1}}
		for _, c := range cases {
			if micros(c.d) != c.micros || millis(c.d) != c.millis {
				t.Errorf("%v: micros = %d, millis = %d, want %d, %d", c.d, micros(c.d), millis(c.d), c.micros, c.millis)
			}
		}
	})
}

type proxy struct {
	listener net.Listener
	target   string
	down     atomic.Bool
	delay    atomic.Int64
	mu       sync.Mutex
	conns    []net.Conn
}

func newProxy(t *testing.T, target string) *proxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{listener: listener, target: target}
	t.Cleanup(func() {
		_ = listener.Close()
		p.cut()
	})
	go p.serve()
	return p
}

func (p *proxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		if p.down.Load() {
			_ = client.Close()
			continue
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, client, server)
		p.mu.Unlock()
		go p.copy(server, client)
		go p.copy(client, server)
	}
}

func (p *proxy) copy(to, from net.Conn) {
	buffer := make([]byte, 32*1024)
	for {
		n, err := from.Read(buffer)
		if n > 0 {
			time.Sleep(time.Duration(p.delay.Load()))
			if _, writeErr := to.Write(buffer[:n]); writeErr != nil {
				err = writeErr
			}
		}
		if err != nil {
			_ = to.Close()
			_ = from.Close()
			return
		}
	}
}

func (p *proxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, conn := range p.conns {
		_ = conn.Close()
	}
	p.conns = nil
}

func (p *proxy) stop() {
	p.down.Store(true)
	p.cut()
}

func (p *proxy) start() { p.down.Store(false) }

type instance struct {
	breaker *circuitbreaker.Breaker
	proxy   *proxy
}

func newInstances(t *testing.T, count int, opts ...circuitbreaker.Option) ([]instance, *Store) {
	t.Helper()
	direct := newStore(t, reachable(t, standalone()))
	instances := make([]instance, count)
	for i := range instances {
		p := newProxy(t, standalone())
		client := redis.NewClient(&redis.Options{Addr: p.listener.Addr().String(), DialTimeout: 200 * time.Millisecond, ReadTimeout: 200 * time.Millisecond, WriteTimeout: 200 * time.Millisecond, MaxRetries: -1})
		t.Cleanup(func() { _ = client.Close() })
		store, err := New(client, WithNamespace(direct.namespace))
		if err != nil {
			t.Fatal(err)
		}
		base := []circuitbreaker.Option{
			circuitbreaker.WithStore(store), circuitbreaker.WithLogger(quiet),
			circuitbreaker.WithFailureThreshold(5), circuitbreaker.WithMinimumCalls(1000),
			circuitbreaker.WithWindowSize(time.Minute), circuitbreaker.WithOpenDuration(300 * time.Millisecond),
			circuitbreaker.WithSyncInterval(40 * time.Millisecond), circuitbreaker.WithStoreTimeout(30 * time.Millisecond),
			circuitbreaker.WithEarlyReportInterval(5 * time.Millisecond), circuitbreaker.WithFaultThreshold(2),
			circuitbreaker.WithProbeAttemptInterval(5 * time.Millisecond), circuitbreaker.WithStartTimeout(2 * time.Second),
		}
		breaker, err := circuitbreaker.New("dependency", append(base, opts...)...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = breaker.Close(context.Background()) })
		instances[i] = instance{breaker: breaker, proxy: p}
	}
	return instances, direct
}

func call(b *circuitbreaker.Breaker, result error) error {
	return b.Do(context.Background(), func(context.Context) error { return result })
}

func all(instances []instance, state circuitbreaker.State, mode circuitbreaker.Mode) func() bool {
	return func() bool {
		for _, i := range instances {
			if i.breaker.State() != state || i.breaker.Mode() != mode {
				return false
			}
		}
		return true
	}
}

func stored(t *testing.T, store *Store) circuitbreaker.Snapshot {
	t.Helper()
	snapshot, err := store.Load(context.Background(), "dependency")
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestAcceptance(t *testing.T) {
	t.Run("AT-1 FD-6 ten instances with one failure each open the circuit through Redis", func(t *testing.T) {
		instances, direct := newInstances(t, 10, circuitbreaker.WithOpenDuration(time.Hour))
		var reached atomic.Int64
		for _, i := range instances {
			_ = i.breaker.Do(context.Background(), func(context.Context) error {
				reached.Add(1)
				return errDependency
			})
		}
		eventually(t, "all instances are open", all(instances, circuitbreaker.Open, circuitbreaker.Distributed))
		if snapshot := stored(t, direct); snapshot.State != circuitbreaker.Open || snapshot.Generation != 1 {
			t.Fatalf("Redis has %+v, want open generation 1", snapshot)
		}
		for _, i := range instances {
			if err := call(i.breaker, nil); err != circuitbreaker.ErrCircuitOpen {
				t.Fatalf("Do() = %v, want ErrCircuitOpen", err)
			}
			if failures := i.breaker.Stats().Failures; failures != 1 {
				t.Fatalf("an instance has %d failures, want 1", failures)
			}
		}
		if reached.Load() != 10 {
			t.Fatalf("the dependency got %d calls, want 10", reached.Load())
		}
	})

	t.Run("AT-2 HO-1 exactly one of ten instances probes through Redis", func(t *testing.T) {
		instances, direct := newInstances(t, 10, circuitbreaker.WithProbeTimeout(time.Hour))
		for range 5 {
			_ = call(instances[0].breaker, errDependency)
		}
		eventually(t, "all instances are open", all(instances, circuitbreaker.Open, circuitbreaker.Distributed))
		time.Sleep(350 * time.Millisecond)
		var permits []circuitbreaker.Permit
		var mu sync.Mutex
		var group sync.WaitGroup
		for _, i := range instances {
			group.Add(1)
			go func() {
				defer group.Done()
				if permit, err := i.breaker.Allow(context.Background()); err == nil {
					mu.Lock()
					permits = append(permits, permit)
					mu.Unlock()
				}
			}()
		}
		group.Wait()
		if len(permits) != 1 {
			t.Fatalf("%d instances got a probe, want 1", len(permits))
		}
		if snapshot := stored(t, direct); snapshot.State != circuitbreaker.HalfOpen || snapshot.Generation != 2 {
			t.Fatalf("Redis has %+v, want half_open generation 2", snapshot)
		}
		permits[0].Success()
		eventually(t, "all instances are closed", all(instances, circuitbreaker.Closed, circuitbreaker.Distributed))
		if snapshot := stored(t, direct); snapshot.Generation != 3 {
			t.Fatalf("Redis has generation %d, want 3", snapshot.Generation)
		}
	})

	t.Run("AT-3 AT-4 DG-1 RC-1 an instance works without Redis and reconciles when Redis is back", func(t *testing.T) {
		instances, direct := newInstances(t, 2, circuitbreaker.WithOpenDuration(time.Hour))
		cut, connected := instances[0], instances[1]
		cut.proxy.stop()
		eventually(t, "local mode", func() bool { return cut.breaker.Mode() == circuitbreaker.Local })
		for range 20 {
			if err := call(cut.breaker, nil); err != nil {
				t.Fatalf("Do() = %v without Redis, want nil", err)
			}
		}
		if s := cut.breaker.Stats(); s.StoreErrors == 0 || s.LocalModeEntries != 1 {
			t.Fatalf("stats = %+v", s)
		}
		for range 5 {
			_ = call(connected.breaker, errDependency)
		}
		eventually(t, "Redis is open", func() bool { return stored(t, direct).State == circuitbreaker.Open })
		if cut.breaker.State() != circuitbreaker.Closed {
			t.Fatalf("the local state = %v, want closed", cut.breaker.State())
		}
		cut.proxy.start()
		eventually(t, "both instances are open in distributed mode", all(instances, circuitbreaker.Open, circuitbreaker.Distributed))
		if snapshot := stored(t, direct); snapshot.State != circuitbreaker.Open || snapshot.Generation != 1 {
			t.Fatalf("Redis has %+v, want open generation 1", snapshot)
		}
	})

	t.Run("AT-6 RC-4 instances with different local states converge after Redis is back", func(t *testing.T) {
		instances, direct := newInstances(t, 4, circuitbreaker.WithOpenDuration(time.Hour), circuitbreaker.WithLocalFailureThreshold(2))
		for _, i := range instances {
			i.proxy.stop()
		}
		eventually(t, "all instances are in local mode", func() bool {
			for _, i := range instances {
				if i.breaker.Mode() != circuitbreaker.Local {
					return false
				}
			}
			return true
		})
		for range 2 {
			_ = call(instances[0].breaker, errDependency)
			_ = call(instances[1].breaker, errDependency)
		}
		_ = call(instances[2].breaker, errDependency)
		if instances[0].breaker.State() != circuitbreaker.Open || instances[3].breaker.State() != circuitbreaker.Closed {
			t.Fatalf("local states = %v, %v, want open, closed", instances[0].breaker.State(), instances[3].breaker.State())
		}
		for _, i := range instances {
			i.proxy.start()
		}
		eventually(t, "all instances are open in distributed mode", all(instances, circuitbreaker.Open, circuitbreaker.Distributed))
		if snapshot := stored(t, direct); snapshot.State != circuitbreaker.Open || snapshot.Generation != 1 {
			t.Fatalf("Redis has %+v, want open generation 1", snapshot)
		}
	})

	t.Run("RF-5 an instance restores the state after Redis lost its data", func(t *testing.T) {
		instances, direct := newInstances(t, 2, circuitbreaker.WithOpenDuration(time.Hour))
		for range 5 {
			_ = call(instances[0].breaker, errDependency)
		}
		eventually(t, "all instances are open", all(instances, circuitbreaker.Open, circuitbreaker.Distributed))
		client := reachable(t, standalone())
		if err := client.Del(context.Background(), direct.keys("dependency")...).Err(); err != nil {
			t.Fatal(err)
		}
		eventually(t, "Redis has the open state again", func() bool {
			snapshot := stored(t, direct)
			return snapshot.State == circuitbreaker.Open && snapshot.Generation == 1
		})
		if !all(instances, circuitbreaker.Open, circuitbreaker.Distributed)() {
			t.Fatal("an instance left the open state")
		}
	})

	t.Run("DG-3 DG-4 a slow Redis delays no call", func(t *testing.T) {
		instances, _ := newInstances(t, 1)
		instances[0].proxy.delay.Store(int64(100 * time.Millisecond))
		eventually(t, "local mode", func() bool { return instances[0].breaker.Mode() == circuitbreaker.Local })
		started := time.Now()
		for range 1000 {
			if err := call(instances[0].breaker, nil); err != nil {
				t.Fatal(err)
			}
		}
		if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
			t.Fatalf("1000 calls took %v with a slow Redis", elapsed)
		}
		instances[0].proxy.delay.Store(0)
		eventually(t, "distributed mode", func() bool { return instances[0].breaker.Mode() == circuitbreaker.Distributed })
	})
}
