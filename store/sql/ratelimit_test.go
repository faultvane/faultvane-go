package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/ratelimit"
	"github.com/faultvane/faultvane-go/ratelimit/storetest"
)

const rateLimitName = "payment-api"

type rateLimitRow struct {
	tokens     float64
	refilledAt int64
	expiresAt  int64
}

func rateLimitStore(t *testing.T, d database, db *sql.DB, opts ...Option) *RateLimitStore {
	t.Helper()
	namespace := fmt.Sprintf("test-%d-%d", time.Now().UnixNano(), namespaceCounter.Add(1))
	store, err := NewRateLimitStore(db, d.dialect, append([]Option{WithNamespace(namespace)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}

func rateLimitStoreWithTable(t *testing.T, d database, db *sql.DB, prefix string) *RateLimitStore {
	t.Helper()
	store := rateLimitStore(t, d, db, WithTable(fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())))
	t.Cleanup(func() { _, _ = db.Exec("DROP TABLE " + store.store.table) })
	return store
}

func rateLimitAcquire(t *testing.T, store ratelimit.Store, name string, request ratelimit.Acquire) ratelimit.Grant {
	t.Helper()
	grant, err := store.Acquire(context.Background(), name, request)
	if err != nil {
		t.Fatal(err)
	}
	return grant
}

func rateLimitRows(t *testing.T, db *sql.DB, store *RateLimitStore, name string) []rateLimitRow {
	t.Helper()
	query := store.store.placeholders("SELECT tokens, refilled_at, expires_at FROM " + store.store.table + " WHERE namespace = ? AND name = ?")
	rows, err := db.Query(query, store.store.namespace, name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var found []rateLimitRow
	for rows.Next() {
		var row rateLimitRow
		if err := rows.Scan(&row.tokens, &row.refilledAt, &row.expiresAt); err != nil {
			t.Fatal(err)
		}
		found = append(found, row)
	}
	return found
}

func rateLimitLimiter(t *testing.T, store ratelimit.Store, opts ...ratelimit.Option) *ratelimit.Limiter {
	t.Helper()
	limiter, err := ratelimit.New(append([]ratelimit.Option{ratelimit.WithStore(store), ratelimit.WithName(rateLimitName), ratelimit.WithLogger(quiet)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = limiter.Close(context.Background()) })
	return limiter
}

func TestRateLimitConformance(t *testing.T) {
	forEachDatabase(t, "RL-21 RL-49 QA-8 the rate limit store contract", func(t *testing.T, d database, db *sql.DB) {
		storetest.Run(t, func(t *testing.T) ratelimit.Store { return rateLimitStore(t, d, db) })
	})
}

func TestRateLimitNew(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://invalid")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	t.Run("RL-49 CFG-3 an invalid database, dialect, namespace, or table is rejected", func(t *testing.T) {
		invalid := map[string]func() (*RateLimitStore, error){
			"nil db":          func() (*RateLimitStore, error) { return NewRateLimitStore(nil, PostgreSQL, WithNamespace("a")) },
			"unknown dialect": func() (*RateLimitStore, error) { return NewRateLimitStore(db, MySQL+1, WithNamespace("a")) },
			"no namespace":    func() (*RateLimitStore, error) { return NewRateLimitStore(db, PostgreSQL) },
			"table with a hyphen": func() (*RateLimitStore, error) {
				return NewRateLimitStore(db, MySQL, WithNamespace("a"), WithTable("a-b"))
			},
		}
		for name, create := range invalid {
			if store, err := create(); store != nil || !errors.Is(err, faultvane.ErrInvalidConfig) {
				t.Errorf("%s: NewRateLimitStore() = %v, %v, want nil and ErrInvalidConfig", name, store, err)
			}
		}
	})

	t.Run("RL-49 the statements of PostgreSQL use numbered parameters, the database time, and the table name", func(t *testing.T) {
		store, err := NewRateLimitStore(db, PostgreSQL, WithNamespace("azAZ09._-"), WithTable("Limits_1"))
		if err != nil {
			t.Fatal(err)
		}
		want := rateLimitQueries{
			schema: "CREATE TABLE IF NOT EXISTS Limits_1 (namespace VARCHAR(64) NOT NULL, name VARCHAR(64) NOT NULL, tokens DOUBLE PRECISION NOT NULL, refilled_at BIGINT NOT NULL, expires_at BIGINT NOT NULL, PRIMARY KEY (namespace, name))",
			insert: "INSERT INTO Limits_1 (namespace, name, tokens, refilled_at, expires_at) VALUES ($1, $2, $3, (EXTRACT(EPOCH FROM clock_timestamp()) * 1000000)::bigint, (EXTRACT(EPOCH FROM clock_timestamp()) * 1000000)::bigint + $4) ON CONFLICT (namespace, name) DO NOTHING",
			lock:   "SELECT tokens, refilled_at, (EXTRACT(EPOCH FROM clock_timestamp()) * 1000000)::bigint FROM Limits_1 WHERE namespace = $1 AND name = $2 FOR UPDATE",
			update: "UPDATE Limits_1 SET tokens = $1, refilled_at = $2, expires_at = $3 WHERE namespace = $4 AND name = $5",
		}
		if store.queries != want || store.Schema() != want.schema {
			t.Fatalf("statements = %+v, want %+v", store.queries, want)
		}
	})

	t.Run("RL-49 the statements of MySQL use question marks, the database time, and the default table", func(t *testing.T) {
		store, err := NewRateLimitStore(db, MySQL, WithNamespace("a"))
		if err != nil {
			t.Fatal(err)
		}
		want := rateLimitQueries{
			schema: "CREATE TABLE IF NOT EXISTS faultvane_rate_limits (namespace VARCHAR(64) NOT NULL, name VARCHAR(64) NOT NULL, tokens DOUBLE PRECISION NOT NULL, refilled_at BIGINT NOT NULL, expires_at BIGINT NOT NULL, PRIMARY KEY (namespace, name))",
			insert: "INSERT IGNORE INTO faultvane_rate_limits (namespace, name, tokens, refilled_at, expires_at) VALUES (?, ?, ?, CAST(UNIX_TIMESTAMP(NOW(6)) * 1000000 AS SIGNED), CAST(UNIX_TIMESTAMP(NOW(6)) * 1000000 AS SIGNED) + ?)",
			lock:   "SELECT tokens, refilled_at, CAST(UNIX_TIMESTAMP(NOW(6)) * 1000000 AS SIGNED) FROM faultvane_rate_limits WHERE namespace = ? AND name = ? FOR UPDATE",
			update: "UPDATE faultvane_rate_limits SET tokens = ?, refilled_at = ?, expires_at = ? WHERE namespace = ? AND name = ?",
		}
		if store.queries != want {
			t.Fatalf("statements = %+v, want %+v", store.queries, want)
		}
	})
}

func TestRateLimitData(t *testing.T) {
	ctx := context.Background()

	forEachDatabase(t, "RL-49 one row for each name holds the tokens, the store time, and the expiry time", func(t *testing.T, d database, db *sql.DB) {
		store := rateLimitStore(t, d, db)
		if err := store.Migrate(ctx); err != nil {
			t.Fatalf("second Migrate() = %v", err)
		}
		grant := rateLimitAcquire(t, store, rateLimitName, ratelimit.Acquire{Wanted: 3, Rate: 1e-9, Burst: 10, StateTTL: time.Hour})
		rateLimitAcquire(t, store, "other", ratelimit.Acquire{Wanted: 1, Rate: 1e-9, Burst: 10, StateTTL: time.Hour})
		at := grant.StoreTime.UnixMicro()
		want := []rateLimitRow{{tokens: 7, refilledAt: at, expiresAt: at + time.Hour.Microseconds()}}
		if got := rateLimitRows(t, db, store, rateLimitName); !slices.Equal(got, want) || grant.Tokens != 3 {
			t.Fatalf("rows = %+v after a grant of %d, want %+v after 3", got, grant.Tokens, want)
		}
		if skew := time.Since(grant.StoreTime).Abs(); skew > time.Hour {
			t.Fatalf("store time %v is %v from the local time", grant.StoreTime, skew)
		}
		later := rateLimitAcquire(t, store, rateLimitName, ratelimit.Acquire{Rate: 1e-9, Burst: 10, StateTTL: 3 * time.Hour})
		got := rateLimitRows(t, db, store, rateLimitName)
		if len(got) != 1 || got[0].refilledAt != later.StoreTime.UnixMicro() || got[0].expiresAt != later.StoreTime.UnixMicro()+(3*time.Hour).Microseconds() {
			t.Fatalf("rows after the second operation = %+v, want one row that expires 3h after %d", got, later.StoreTime.UnixMicro())
		}
	})

	forEachDatabase(t, "RL-49 a part of a token stays in the bucket", func(t *testing.T, d database, db *sql.DB) {
		store := rateLimitStore(t, d, db)
		first := rateLimitAcquire(t, store, rateLimitName, ratelimit.Acquire{Wanted: 10, Rate: 1e-9, Burst: 10, StateTTL: time.Hour})
		var second ratelimit.Grant
		eventually(t, "the time of the database moves", func() bool {
			second = rateLimitAcquire(t, store, rateLimitName, ratelimit.Acquire{Rate: 0.5, Burst: 10, StateTTL: time.Hour})
			return second.StoreTime.After(first.StoreTime)
		})
		want := 0.5 * second.StoreTime.Sub(first.StoreTime).Seconds()
		if got := rateLimitRows(t, db, store, rateLimitName)[0].tokens; second.Tokens != 0 || got <= 0 || got > want*1.000001 {
			t.Fatalf("tokens in the database = %v with a grant of %d, want above 0 and at most %v with no grant", got, second.Tokens, want)
		}
	})

	forEachDatabase(t, "RL-49 the bucket refills by the time that the database returns, and this time does not go back", func(t *testing.T, d database, db *sql.DB) {
		store := rateLimitStore(t, d, db)
		request := ratelimit.Acquire{Wanted: 10, Rate: 0.001, Burst: 10, StateTTL: time.Hour}
		drained := rateLimitAcquire(t, store, rateLimitName, request)
		ahead := *store
		ahead.queries.lock = strings.Replace(store.queries.lock, store.store.now(), "("+store.store.now()+" + 3600000000)", 1)
		refilled := rateLimitAcquire(t, &ahead, rateLimitName, request)
		if elapsed := refilled.StoreTime.Sub(drained.StoreTime); refilled.Tokens != 3 || elapsed < time.Hour || elapsed > time.Hour+time.Minute {
			t.Fatalf("grant = %+v, want 3 tokens 1h after %v", refilled, drained.StoreTime)
		}
		back := rateLimitAcquire(t, store, rateLimitName, request)
		if back.Tokens != 0 || !back.StoreTime.Equal(refilled.StoreTime) {
			t.Fatalf("grant after the time went back = %+v, want no tokens at %v", back, refilled.StoreTime)
		}
		if got := rateLimitRows(t, db, store, rateLimitName)[0]; got.tokens > 0.7 || got.refilledAt != refilled.StoreTime.UnixMicro() {
			t.Fatalf("row = %+v, want less than 1 token at %d", got, refilled.StoreTime.UnixMicro())
		}
	})

	forEachDatabase(t, "RL-49 concurrent first operations of new names grant exactly the burst", func(t *testing.T, d database, db *sql.DB) {
		store := rateLimitStore(t, d, db)
		const names, burst = 4, 100
		var granted [names]atomic.Int64
		var failed atomic.Value
		var group sync.WaitGroup
		for i := range names * 12 {
			group.Add(1)
			go func() {
				defer group.Done()
				grant, err := store.Acquire(ctx, fmt.Sprintf("new-%d", i%names), ratelimit.Acquire{Wanted: 10, Rate: 1e-9, Burst: burst, StateTTL: time.Hour})
				if err != nil {
					failed.Store(err.Error())
				}
				granted[i%names].Add(grant.Tokens)
			}()
		}
		group.Wait()
		for i := range granted {
			if got := granted[i].Load(); got != burst || failed.Load() != nil {
				t.Fatalf("name %d: %d tokens granted with the error %v, want %d and no error", i, got, failed.Load(), burst)
			}
			if rows := rateLimitRows(t, db, store, fmt.Sprintf("new-%d", i)); len(rows) != 1 || rows[0].tokens > 1e-6 {
				t.Fatalf("name %d: rows = %+v, want one empty bucket", i, rows)
			}
		}
	})

	forEachDatabase(t, "RL-51 cleanup removes only rows that are not in use", func(t *testing.T, d database, db *sql.DB) {
		store := rateLimitStoreWithTable(t, d, db, "rl_cleanup")
		used := rateLimitAcquire(t, store, "used", ratelimit.Acquire{Wanted: 4, Rate: 1e-9, Burst: 10, StateTTL: time.Hour})
		rateLimitAcquire(t, store, "unused", ratelimit.Acquire{Wanted: 4, Rate: 1e-9, Burst: 10, StateTTL: time.Millisecond})
		eventually(t, "the unused row expired", func() bool {
			return rateLimitAcquire(t, store, "used", ratelimit.Acquire{Rate: 1e-9, Burst: 10, StateTTL: time.Hour}).StoreTime.Sub(used.StoreTime) > 5*time.Millisecond
		})
		removed, err := store.Cleanup(ctx)
		if err != nil || removed != 1 {
			t.Fatalf("Cleanup() = %d, %v, want 1 row", removed, err)
		}
		if rows := rateLimitRows(t, db, store, "used"); len(rows) != 1 || math.Abs(rows[0].tokens-6) > 1e-6 {
			t.Fatalf("rows of the used name = %+v, want one row with 6 tokens", rows)
		}
		if rows := rateLimitRows(t, db, store, "unused"); len(rows) != 0 {
			t.Fatalf("rows of the unused name = %+v, want no row", rows)
		}
		if grant := rateLimitAcquire(t, store, "unused", ratelimit.Acquire{Wanted: 10, Rate: 1e-9, Burst: 10, StateTTL: time.Hour}); grant.Tokens != 10 {
			t.Fatalf("grant after the cleanup = %+v, want a full bucket", grant)
		}
	})

	forEachDatabase(t, "RL-51 an operation runs the cleanup at most one time in each interval", func(t *testing.T, d database, db *sql.DB) {
		store := rateLimitStoreWithTable(t, d, db, "rl_interval")
		count := func() (rows int) {
			_ = db.QueryRow("SELECT COUNT(*) FROM " + store.store.table).Scan(&rows)
			return rows
		}
		expire := func() {
			t.Helper()
			if _, err := db.Exec("UPDATE " + store.store.table + " SET expires_at = 1"); err != nil {
				t.Fatal(err)
			}
		}
		insert := store.store.placeholders("INSERT INTO " + store.store.table + " (namespace, name, tokens, refilled_at, expires_at) VALUES (?, 'old', 1, 1, 1)")
		if _, err := db.Exec(insert, store.store.namespace); err != nil {
			t.Fatal(err)
		}
		request := ratelimit.Acquire{Wanted: 1, Rate: 1e-9, Burst: 10, StateTTL: time.Hour}
		if rateLimitAcquire(t, store, "a", request); count() != 1 {
			t.Fatalf("rows = %d, want 1 row after the first operation", count())
		}
		expire()
		if rateLimitAcquire(t, store, "b", request); count() != 2 {
			t.Fatalf("rows = %d, want 2 rows inside the cleanup interval", count())
		}
		store.store.lastCleanup.Add(-cleanupInterval.Microseconds())
		expire()
		if rateLimitAcquire(t, store, "c", request); count() != 1 {
			t.Fatalf("rows = %d, want 1 row after the cleanup interval", count())
		}
	})
}

func TestRateLimitFaults(t *testing.T) {
	ctx := context.Background()
	request := ratelimit.Acquire{Wanted: 1, Rate: 1, Burst: 1, StateTTL: time.Hour}

	forEachDatabase(t, "RL-50 each operation on a closed database returns ErrStoreUnavailable", func(t *testing.T, d database, db *sql.DB) {
		closed, err := sql.Open(d.driver, d.dsn)
		if err != nil {
			t.Fatal(err)
		}
		store := rateLimitStore(t, d, closed)
		_ = closed.Close()
		grant, acquireErr := store.Acquire(ctx, rateLimitName, request)
		_, cleanupErr := store.Cleanup(ctx)
		failures := map[string]error{"rate limit acquire": acquireErr, "cleanup": cleanupErr, "migrate": store.Migrate(ctx)}
		for operation, err := range failures {
			if !errors.Is(err, faultvane.ErrStoreUnavailable) || !strings.HasPrefix(err.Error(), "sqlstore: "+operation+": faultvane: store unavailable: ") {
				t.Errorf("%s: error = %v", operation, err)
			}
		}
		if grant != (ratelimit.Grant{}) {
			t.Errorf("grant of a failed operation = %+v", grant)
		}
	})

	forEachDatabase(t, "RL-50 a table that does not exist is a store fault", func(t *testing.T, d database, db *sql.DB) {
		store, err := NewRateLimitStore(db, d.dialect, WithNamespace("a"), WithTable("missing_table"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Acquire(ctx, rateLimitName, request); !errors.Is(err, faultvane.ErrStoreUnavailable) {
			t.Fatalf("Acquire() error = %v, want ErrStoreUnavailable", err)
		}
	})

	forEachDatabase(t, "RL-50 an operation stops when its context ends and changes nothing", func(t *testing.T, d database, db *sql.DB) {
		store := rateLimitStore(t, d, db)
		ended, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := store.Acquire(ended, rateLimitName, request); !errors.Is(err, context.Canceled) || !errors.Is(err, faultvane.ErrStoreUnavailable) {
			t.Fatalf("Acquire() error = %v, want context.Canceled and ErrStoreUnavailable", err)
		}
		if rows := rateLimitRows(t, db, store, rateLimitName); len(rows) != 0 {
			t.Fatalf("rows = %+v, want no row", rows)
		}
	})

	forEachDatabase(t, "RL-50 an operation whose write fails returns ErrStoreUnavailable and changes nothing", func(t *testing.T, d database, db *sql.DB) {
		store := rateLimitStoreWithTable(t, d, db, "rl_write")
		if _, err := db.Exec("ALTER TABLE " + store.store.table + " ADD CONSTRAINT " + store.store.table + "_check CHECK (expires_at > 0)"); err != nil {
			t.Fatal(err)
		}
		rateLimitAcquire(t, store, rateLimitName, ratelimit.Acquire{Wanted: 3, Rate: 1e-9, Burst: 10, StateTTL: time.Hour})
		before := rateLimitRows(t, db, store, rateLimitName)
		grant, err := store.Acquire(ctx, rateLimitName, ratelimit.Acquire{Wanted: 3, Rate: 1e-9, Burst: 10, StateTTL: -1000000 * time.Hour})
		if !errors.Is(err, faultvane.ErrStoreUnavailable) || grant != (ratelimit.Grant{}) {
			t.Fatalf("Acquire() = %+v, %v, want no grant and ErrStoreUnavailable", grant, err)
		}
		if after := rateLimitRows(t, db, store, rateLimitName); !slices.Equal(after, before) {
			t.Fatalf("rows = %+v, want %+v", after, before)
		}
	})
}

type rateLimitCounter struct {
	ratelimit.Store
	calls atomic.Int64
}

func (s *rateLimitCounter) Acquire(ctx context.Context, name string, request ratelimit.Acquire) (ratelimit.Grant, error) {
	s.calls.Add(1)
	return s.Store.Acquire(ctx, name, request)
}

func rateLimitStoreTime(t *testing.T, store ratelimit.Store) time.Time {
	t.Helper()
	return rateLimitAcquire(t, store, "clock", ratelimit.Acquire{Rate: 1, Burst: 1, StateTTL: time.Minute}).StoreTime
}

func TestRateLimitAcceptance(t *testing.T) {
	forEachDatabase(t, "RL-52 RL-26 RL-23 ten limiters under heavy load share one rate", func(t *testing.T, d database, db *sql.DB) {
		const (
			rate     = 500.0
			burst    = 50
			duration = 2 * time.Second
		)
		store := rateLimitStore(t, d, db)
		counting := &rateLimitCounter{Store: store}
		started := rateLimitStoreTime(t, store)
		var permitted atomic.Int64
		var group sync.WaitGroup
		stop := make(chan struct{})
		limiters := make([]*ratelimit.Limiter, 10)
		for i := range limiters {
			limiter := rateLimitLimiter(t, counting, ratelimit.WithRate(rate), ratelimit.WithBurst(burst), ratelimit.WithLocalRate(1e-6), ratelimit.WithFaultThreshold(1<<30))
			limiters[i] = limiter
			group.Add(1)
			go func() {
				defer group.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					if limiter.Allow() {
						permitted.Add(1)
					}
					time.Sleep(50 * time.Microsecond)
				}
			}()
		}
		time.Sleep(duration)
		close(stop)
		group.Wait()
		elapsed := rateLimitStoreTime(t, store).Sub(started).Seconds()
		calls, faults := int64(0), int64(0)
		for _, limiter := range limiters {
			stats := limiter.Stats()
			calls += stats.Permitted + stats.Rejected
			faults += stats.StoreErrors
		}
		got, most, least := float64(permitted.Load()), burst+rate*elapsed+float64(len(limiters)), 0.7*rate*elapsed
		if got > most || got < least {
			t.Fatalf("permitted calls = %v of %d in %.3f s with %d store faults, want from %.0f to %.0f", got, calls, elapsed, faults, least, most)
		}
		if operations, limit := counting.calls.Load(), int64(len(limiters))*(2+int64(elapsed/0.1)); operations > limit || calls < 10*operations {
			t.Fatalf("store operations = %d for %d calls, want at most %d", operations, calls, limit)
		}
	})

	forEachDatabase(t, "RL-53 RL-27 RL-29 the limiters use the local rate during a database outage and share the rate again after it", func(t *testing.T, d database, db *sql.DB) {
		const (
			localRate  = 100.0
			localBurst = 10
		)
		namespace := rateLimitStore(t, d, db).store.namespace
		links := make([]*link, 3)
		limiters := make([]*ratelimit.Limiter, len(links))
		for i := range limiters {
			links[i] = &link{}
			store, err := NewRateLimitStore(openThrough(t, d, links[i]), d.dialect, WithNamespace(namespace))
			if err != nil {
				t.Fatal(err)
			}
			limiters[i] = rateLimitLimiter(t, store, ratelimit.WithRate(1000), ratelimit.WithBurst(100), ratelimit.WithLocalRate(localRate),
				ratelimit.WithSyncInterval(100*time.Millisecond), ratelimit.WithStoreTimeout(90*time.Millisecond), ratelimit.WithFaultThreshold(2))
		}
		all := func(mode ratelimit.Mode) func() bool {
			return func() bool {
				for _, limiter := range limiters {
					_ = limiter.Allow()
					if limiter.Stats().Mode != mode {
						return false
					}
				}
				return true
			}
		}
		eventually(t, "all limiters are in distributed mode", all(ratelimit.Distributed))
		for _, l := range links {
			l.stop()
		}
		eventually(t, "all limiters are in local mode", all(ratelimit.Local))
		for i, limiter := range limiters {
			started, count := time.Now(), 0
			for count < 3*localBurst {
				if limiter.Allow() {
					count++
				}
				if time.Since(started) > 15*time.Second {
					t.Fatalf("limiter %d permitted %d calls in local mode, want %d", i, count, 3*localBurst)
				}
				time.Sleep(100 * time.Microsecond)
			}
			if most := localBurst + localRate*time.Since(started).Seconds(); float64(count) > most {
				t.Fatalf("limiter %d permitted %d calls in local mode, want at most %.1f", i, count, most)
			}
			if stats := limiter.Stats(); stats.Mode != ratelimit.Local || stats.StoreErrors < 2 {
				t.Fatalf("limiter %d: stats = %+v, want local mode after 2 store errors", i, stats)
			}
		}
		for _, l := range links {
			l.start()
		}
		eventually(t, "all limiters are in distributed mode again", all(ratelimit.Distributed))
		eventually(t, "a limiter permits a call from the shared bucket", func() bool {
			return limiters[0].Allow() && limiters[0].Stats().Mode == ratelimit.Distributed
		})
	})
}

func BenchmarkRateLimitAcquire(b *testing.B) {
	for _, d := range databases() {
		db, err := sql.Open(d.driver, d.dsn)
		if err != nil {
			b.Fatal(err)
		}
		store, err := NewRateLimitStore(db, d.dialect, WithNamespace(fmt.Sprintf("bench-%d", time.Now().UnixNano())))
		if err != nil {
			b.Fatal(err)
		}
		if err := store.Migrate(context.Background()); err != nil {
			b.Fatal(err)
		}
		b.Run(d.name, func(b *testing.B) {
			var durations []time.Duration
			for b.Loop() {
				started := time.Now()
				if _, err := store.Acquire(context.Background(), "a", ratelimit.Acquire{Wanted: 1, Rate: 1000, Burst: 100, StateTTL: time.Hour}); err != nil {
					b.Fatal(err)
				}
				durations = append(durations, time.Since(started))
			}
			slices.Sort(durations)
			b.ReportMetric(float64(durations[len(durations)/2].Microseconds())/1000, "p50-ms")
			b.ReportMetric(float64(durations[len(durations)*99/100].Microseconds())/1000, "p99-ms")
		})
		_ = db.Close()
	}
}
